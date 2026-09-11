package systeminfo

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/flightctl/flightctl/api/core/v1beta1"
	"github.com/flightctl/flightctl/internal/agent/config"
	"github.com/flightctl/flightctl/internal/agent/device/errors"
	"github.com/flightctl/flightctl/internal/agent/device/fileio"
	"github.com/flightctl/flightctl/internal/agent/device/status"
	"github.com/flightctl/flightctl/pkg/executer"
	"github.com/flightctl/flightctl/pkg/log"
	"github.com/flightctl/flightctl/pkg/version"
	"github.com/samber/lo"
)

// sourceCategory classifies a collection source as built-in or custom.
type sourceCategory int

const (
	sourceCategoryBuiltIn sourceCategory = iota
	sourceCategoryCustom
)

// cachedSource is the manager's in-memory record for one source. It holds the
// last successful value, current success/failure state, agent-generated message,
// and last transition time. Never exported or persisted.
type cachedSource struct {
	name     string
	category sourceCategory

	value    string
	hasValue bool

	failed  bool
	message string

	attempted          bool
	lastTransitionTime time.Time
}

// recordSuccess records a successful collection. Transition time advances only
// on first attempt or when recovering from failure.
func (cs *cachedSource) recordSuccess(now time.Time, value string) {
	transitioned := !cs.attempted || cs.failed
	cs.attempted = true
	cs.value = value
	cs.hasValue = true
	cs.failed = false
	cs.message = ""
	if transitioned {
		cs.lastTransitionTime = now
	}
}

// recordFailure records a failed collection. The cached value is retained
// (value retention on failure). Transition time advances only on first attempt
// or when transitioning from success.
func (cs *cachedSource) recordFailure(now time.Time, message string) {
	transitioned := !cs.attempted || !cs.failed
	cs.attempted = true
	cs.failed = true
	cs.message = message
	if transitioned {
		cs.lastTransitionTime = now
	}
}

// clearValue clears the cached value (used for allow-listed scripts not found).
func (cs *cachedSource) clearValue(now time.Time, message string) {
	cs.recordFailure(now, message)
	cs.value = ""
	cs.hasValue = false
}

// reconcileSources matches desired source keys against existing cached state.
// New keys get fresh entries, removed keys are dropped, existing keys keep state.
func reconcileSources(existing []cachedSource, builtInKeys, customKeys []string) []cachedSource {
	type cacheKey struct {
		name     string
		category sourceCategory
	}
	index := make(map[cacheKey]int, len(existing))
	for i, cs := range existing {
		index[cacheKey{cs.name, cs.category}] = i
	}

	desired := make([]cachedSource, 0, len(builtInKeys)+len(customKeys))
	for _, key := range builtInKeys {
		ck := cacheKey{key, sourceCategoryBuiltIn}
		if idx, ok := index[ck]; ok {
			desired = append(desired, existing[idx])
		} else {
			desired = append(desired, cachedSource{name: key, category: sourceCategoryBuiltIn})
		}
	}
	for _, key := range customKeys {
		ck := cacheKey{key, sourceCategoryCustom}
		if idx, ok := index[ck]; ok {
			desired = append(desired, existing[idx])
		} else {
			desired = append(desired, cachedSource{name: key, category: sourceCategoryCustom})
		}
	}
	return desired
}

// buildSystemInfoStatus constructs the DeviceSystemInfoStatus from cached sources.
func buildSystemInfoStatus(sources []cachedSource) v1beta1.DeviceSystemInfoStatus {
	sysMap := make(map[string]v1beta1.SystemInfoSourceStatus)
	customMap := make(map[string]v1beta1.SystemInfoSourceStatus)

	for i := range sources {
		cs := &sources[i]
		if !cs.attempted {
			continue
		}
		entry := v1beta1.SystemInfoSourceStatus{
			LastTransitionTime: cs.lastTransitionTime,
		}
		if cs.failed && cs.message != "" {
			entry.Message = lo.ToPtr(cs.message)
		}
		switch cs.category {
		case sourceCategoryBuiltIn:
			sysMap[cs.name] = entry
		case sourceCategoryCustom:
			customMap[cs.name] = entry
		}
	}

	return v1beta1.DeviceSystemInfoStatus{
		Summary: v1beta1.DeviceSystemInfoSummaryStatus{
			Status: computeSummaryStatus(sources),
		},
		Statuses: v1beta1.DeviceSystemInfoStatuses{
			SystemInfo: sysMap,
			CustomInfo: customMap,
		},
	}
}

// computeSummaryStatus derives aggregate health. Unknown if any source has not
// been attempted; otherwise Healthy/Degraded/Error based on failure counts.
func computeSummaryStatus(sources []cachedSource) v1beta1.SystemInfoSummaryStatusType {
	if len(sources) == 0 {
		return v1beta1.SystemInfoSummaryStatusUnknown
	}
	total, failed := 0, 0
	for i := range sources {
		if !sources[i].attempted {
			return v1beta1.SystemInfoSummaryStatusUnknown
		}
		total++
		if sources[i].failed {
			failed++
		}
	}
	switch {
	case failed == 0:
		return v1beta1.SystemInfoSummaryStatusHealthy
	case failed == total:
		return v1beta1.SystemInfoSummaryStatusError
	default:
		return v1beta1.SystemInfoSummaryStatusDegraded
	}
}

// collectAndBuildStatus is the manager's private stateful collection path (PATH B).
// It runs built-in and custom collectors, records outcomes in the cached source
// state, and returns DeviceSystemInfo + DeviceSystemInfoStatus.
//
// The manager lock must NOT be held when calling this — script execution happens
// here. The caller must snapshot config under the lock before calling.
func collectAndBuildStatus(
	ctx context.Context,
	lg *log.PrefixLogger,
	exec executer.Executer,
	reader fileio.ReadWriter,
	infoKeys []string,
	customKeys []string,
	bootID string,
	runtimeCollectors map[string]CollectorFn,
	hardwareMapPath string,
	sources []cachedSource,
) (v1beta1.DeviceSystemInfo, v1beta1.DeviceSystemInfoStatus, []cachedSource) {
	now := time.Now()
	agentVer := version.Get()

	// Determine desired custom keys for this cycle
	desiredCustom := discoverDesiredCustomKeys(reader, customKeys, lg)

	// Reconcile cached sources with current desired set
	sources = reconcileSources(sources, infoKeys, desiredCustom)

	// ---- Built-in collection (uses existing Collect path internally) ----
	collectionOpts, err := collectionOptsFromInfoKeys(infoKeys)
	if err != nil {
		lg.Warnf("Failed to handle system info keys: %v", err)
	}

	info, err := Collect(ctx, lg, exec, reader, nil, hardwareMapPath, collectionOpts...)
	if err != nil {
		lg.Errorf("Failed to collect system info: %v", err)
		// Return defaults with current source state
		return defaultSystemInfoFor(bootID, agentVer.GitVersion), buildSystemInfoStatus(sources), sources
	}

	// Build the system info map from the collected Info + runtime collectors
	systemInfoMap := getSystemInfoMap(ctx, lg, info, infoKeys, runtimeCollectors)

	// Record built-in outcomes
	for i := range sources {
		cs := &sources[i]
		if cs.category != sourceCategoryBuiltIn {
			continue
		}
		if val, exists := systemInfoMap[cs.name]; exists {
			cs.recordSuccess(now, val)
		} else {
			// Key not in map means collector ran without producing it or wasn't run
			cs.recordSuccess(now, "")
		}
	}

	// ---- Custom collection (independent from Collect's custom path) ----
	collectCustomSources(ctx, lg, exec, reader, sources, now)

	// Build output
	additionalProperties := make(map[string]string, len(systemInfoMap))
	for k, v := range systemInfoMap {
		additionalProperties[k] = v
	}

	sysInfo := v1beta1.DeviceSystemInfo{
		Architecture:         info.Architecture,
		OperatingSystem:      info.OperatingSystem,
		BootID:               bootID,
		AgentVersion:         agentVer.GitVersion,
		AdditionalProperties: additionalProperties,
	}

	// Build custom info from cached values (preserves last-good on failure)
	customValues := make(map[string]string)
	for i := range sources {
		cs := &sources[i]
		if cs.category == sourceCategoryCustom && cs.hasValue {
			customValues[cs.name] = cs.value
		}
	}
	if len(customValues) > 0 {
		sysInfo.CustomInfo = lo.ToPtr(v1beta1.CustomDeviceInfo(customValues))
	}

	return sysInfo, buildSystemInfoStatus(sources), sources
}

// collectCustomSources runs custom script collection for all custom entries in
// the source cache. Uses agent-generated messages, never raw stderr.
func collectCustomSources(
	ctx context.Context,
	lg *log.PrefixLogger,
	exec executer.Executer,
	reader fileio.Reader,
	sources []cachedSource,
	now time.Time,
) {
	exists, err := reader.PathExists(config.SystemInfoCustomScriptDir)
	if err != nil || !exists {
		// Mark all custom sources as "script not found"
		for i := range sources {
			if sources[i].category == sourceCategoryCustom {
				sources[i].clearValue(now, truncateMessage("script not found"))
			}
		}
		return
	}

	entries, err := os.ReadDir(reader.PathFor(config.SystemInfoCustomScriptDir))
	if err != nil {
		for i := range sources {
			if sources[i].category == sourceCategoryCustom {
				sources[i].clearValue(now, truncateMessage("script not found"))
			}
		}
		return
	}

	for i := range sources {
		cs := &sources[i]
		if cs.category != sourceCategoryCustom {
			continue
		}
		if ctx.Err() != nil {
			// Context canceled — don't attempt remaining sources
			return
		}
		runCustomScript(ctx, cs, reader, exec, entries, now)
	}
}

// runCustomScript executes a single custom script and records the outcome.
func runCustomScript(
	ctx context.Context,
	cs *cachedSource,
	reader fileio.Reader,
	exec executer.Executer,
	entries []fs.DirEntry,
	now time.Time,
) {
	val, err := getCustomInfoValue(ctx, cs.name, reader, exec, entries)
	if err != nil {
		msg := classifyCustomError(err)
		if cs.hasValue {
			// Retain last-good value on failure
			cs.recordFailure(now, msg)
		} else {
			cs.clearValue(now, msg)
		}
		return
	}

	// getCustomInfoValue returns ("", nil) when no script is found
	// Distinguish: if val is empty AND no candidates matched, it's "not found"
	// But getCustomInfoValue returns "" with nil error for both "no candidates"
	// and "successful empty output". We check candidates to differentiate.
	if val == "" && !hasScriptCandidate(cs.name, entries) {
		cs.clearValue(now, truncateMessage("script not found"))
		return
	}

	cs.recordSuccess(now, val)
}

// hasScriptCandidate checks if any directory entry matches the key pattern.
func hasScriptCandidate(key string, entries []fs.DirEntry) bool {
	keyLower := strings.ToLower(key)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		base := strings.TrimSuffix(name, filepath.Ext(name))
		baseLower := strings.ToLower(base)
		if base == key || baseLower == keyLower ||
			strings.HasSuffix(base, "-"+key) || strings.HasSuffix(baseLower, "-"+keyLower) {
			return true
		}
	}
	return false
}

// classifyCustomError produces agent-generated status message from script error.
func classifyCustomError(err error) string {
	if errors.IsContext(err) {
		return truncateMessage("collection exceeded deadline")
	}
	msg := err.Error()
	if code := extractExitCode(msg); code > 0 {
		return truncateMessage(fmt.Sprintf("script exited with status %d", code))
	}
	return truncateMessage("script exited with status 1")
}

// extractExitCode parses the exit code from a FromStderr-formatted error.
func extractExitCode(msg string) int {
	idx := strings.Index(msg, "code: ")
	if idx < 0 {
		return 0
	}
	rest := msg[idx+len("code: "):]
	var code int
	if _, err := fmt.Sscanf(rest, "%d", &code); err == nil {
		return code
	}
	return 0
}

// truncateMessage bounds a status message to status.MaxMessageLength.
func truncateMessage(msg string) string {
	return log.Truncate(msg, status.MaxMessageLength)
}

// discoverDesiredCustomKeys determines the custom keys for this cycle. If
// customKeys is non-empty, those are the allow-listed keys. Otherwise discover
// executable scripts from the custom script directory.
func discoverDesiredCustomKeys(reader fileio.Reader, customKeys []string, lg *log.PrefixLogger) []string {
	if len(customKeys) > 0 {
		return customKeys
	}
	exists, err := reader.PathExists(config.SystemInfoCustomScriptDir)
	if err != nil || !exists {
		return nil
	}
	entries, err := os.ReadDir(reader.PathFor(config.SystemInfoCustomScriptDir))
	if err != nil {
		lg.Debugf("Failed to read custom info directory: %v", err)
		return nil
	}
	return discoverExecutableKeys(entries, reader)
}

// discoverExecutableKeys returns keys for entries that are executable files.
func discoverExecutableKeys(entries []os.DirEntry, reader fileio.Reader) []string {
	var keys []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		scriptPath := filepath.Join(reader.PathFor(config.SystemInfoCustomScriptDir), name)
		info, err := os.Stat(scriptPath)
		if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		if base != "" {
			keys = append(keys, base)
		}
	}
	sort.Strings(keys)
	return keys
}

// defaultSystemInfoFor returns minimal system info for error cases.
func defaultSystemInfoFor(bootID, agentVersion string) v1beta1.DeviceSystemInfo {
	return v1beta1.DeviceSystemInfo{
		BootID:               bootID,
		AgentVersion:         agentVersion,
		OperatingSystem:      "linux",
		Architecture:         "amd64",
		AdditionalProperties: make(map[string]string),
	}
}

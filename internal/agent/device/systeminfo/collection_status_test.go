package systeminfo

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/flightctl/flightctl/api/core/v1beta1"
	"github.com/stretchr/testify/require"
)

func Test_cachedSource_transitions(t *testing.T) {
	require := require.New(t)
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	t2 := t0.Add(2 * time.Minute)
	t3 := t0.Add(3 * time.Minute)
	t4 := t0.Add(4 * time.Minute)

	t.Run("When first attempt succeeds it should set transition time", func(t *testing.T) {
		cs := cachedSource{name: "a", category: sourceCategoryBuiltIn}
		cs.recordSuccess(t0, "v1")

		require.True(cs.attempted)
		require.False(cs.failed)
		require.True(cs.hasValue)
		require.Equal("v1", cs.value)
		require.Equal(t0, cs.lastTransitionTime)
	})

	t.Run("When value changes but success continues it should keep original transition time", func(t *testing.T) {
		cs := cachedSource{name: "a", category: sourceCategoryBuiltIn}
		cs.recordSuccess(t0, "v1")
		cs.recordSuccess(t1, "v2")

		require.Equal("v2", cs.value)
		require.Equal(t0, cs.lastTransitionTime)
	})

	t.Run("When success transitions to failure it should advance transition time and retain value", func(t *testing.T) {
		cs := cachedSource{name: "a", category: sourceCategoryCustom}
		cs.recordSuccess(t0, "v1")
		cs.recordFailure(t1, "script exited with status 1")

		require.True(cs.failed)
		require.Equal("v1", cs.value)
		require.True(cs.hasValue)
		require.Equal(t1, cs.lastTransitionTime)
	})

	t.Run("When repeated failure it should keep failure transition time", func(t *testing.T) {
		cs := cachedSource{name: "a", category: sourceCategoryCustom}
		cs.recordSuccess(t0, "v1")
		cs.recordFailure(t1, "msg1")
		cs.recordFailure(t2, "msg2")

		require.Equal("msg2", cs.message)
		require.Equal(t1, cs.lastTransitionTime)
	})

	t.Run("When failure recovers it should advance transition time", func(t *testing.T) {
		cs := cachedSource{name: "a", category: sourceCategoryCustom}
		cs.recordSuccess(t0, "v1")
		cs.recordFailure(t1, "failed")
		cs.recordSuccess(t2, "v2")

		require.False(cs.failed)
		require.Equal("v2", cs.value)
		require.Equal(t2, cs.lastTransitionTime)
	})

	t.Run("Full lifecycle: success -> failure -> stable failure -> recovery -> stable success", func(t *testing.T) {
		cs := cachedSource{name: "lc", category: sourceCategoryCustom}

		cs.recordSuccess(t0, "v1")
		require.Equal(t0, cs.lastTransitionTime)

		cs.recordFailure(t1, "failed")
		require.Equal(t1, cs.lastTransitionTime)
		require.Equal("v1", cs.value)

		cs.recordFailure(t2, "still failed")
		require.Equal(t1, cs.lastTransitionTime)

		cs.recordSuccess(t3, "v2")
		require.Equal(t3, cs.lastTransitionTime)

		cs.recordSuccess(t4, "v3")
		require.Equal(t3, cs.lastTransitionTime)
		require.Equal("v3", cs.value)
	})

	t.Run("When clearValue is called it should remove value and mark failed", func(t *testing.T) {
		cs := cachedSource{name: "c", category: sourceCategoryCustom}
		cs.recordSuccess(t0, "old-value")
		cs.clearValue(t1, "script not found")

		require.True(cs.failed)
		require.False(cs.hasValue)
		require.Equal("", cs.value)
		require.Equal("script not found", cs.message)
	})
}

func Test_buildSystemInfoStatus(t *testing.T) {
	require := require.New(t)
	now := time.Now()

	tests := []struct {
		name           string
		sources        []cachedSource
		expectedStatus v1beta1.SystemInfoSummaryStatusType
		sysInfoCount   int
		customCount    int
	}{
		{
			name: "When all succeed it should report Healthy",
			sources: []cachedSource{
				{name: "cpuCores", category: sourceCategoryBuiltIn, attempted: true, lastTransitionTime: now},
				{name: "myScript", category: sourceCategoryCustom, attempted: true, hasValue: true, lastTransitionTime: now},
			},
			expectedStatus: v1beta1.SystemInfoSummaryStatusHealthy,
			sysInfoCount:   1,
			customCount:    1,
		},
		{
			name: "When all fail it should report Error",
			sources: []cachedSource{
				{name: "cpuCores", category: sourceCategoryBuiltIn, attempted: true, failed: true, message: "err", lastTransitionTime: now},
				{name: "myScript", category: sourceCategoryCustom, attempted: true, failed: true, message: "err", lastTransitionTime: now},
			},
			expectedStatus: v1beta1.SystemInfoSummaryStatusError,
			sysInfoCount:   1,
			customCount:    1,
		},
		{
			name: "When mixed it should report Degraded",
			sources: []cachedSource{
				{name: "cpuCores", category: sourceCategoryBuiltIn, attempted: true, lastTransitionTime: now},
				{name: "bad", category: sourceCategoryCustom, attempted: true, failed: true, message: "err", lastTransitionTime: now},
			},
			expectedStatus: v1beta1.SystemInfoSummaryStatusDegraded,
			sysInfoCount:   1,
			customCount:    1,
		},
		{
			name:           "When no sources it should report Unknown",
			sources:        []cachedSource{},
			expectedStatus: v1beta1.SystemInfoSummaryStatusUnknown,
		},
		{
			name: "When unattempted source exists it should report Unknown",
			sources: []cachedSource{
				{name: "cpuCores", category: sourceCategoryBuiltIn, attempted: true, lastTransitionTime: now},
				{name: "notYet", category: sourceCategoryCustom, attempted: false},
			},
			expectedStatus: v1beta1.SystemInfoSummaryStatusUnknown,
			sysInfoCount:   1,
			customCount:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := buildSystemInfoStatus(tt.sources)
			require.Equal(tt.expectedStatus, status.Summary.Status)
			require.Len(status.Statuses.SystemInfo, tt.sysInfoCount)
			require.Len(status.Statuses.CustomInfo, tt.customCount)
		})
	}
}

func Test_buildSystemInfoStatus_errorMessages(t *testing.T) {
	require := require.New(t)
	now := time.Now()

	t.Run("When a source fails it should include error message", func(t *testing.T) {
		sources := []cachedSource{
			{name: "fail", category: sourceCategoryCustom, attempted: true, failed: true, message: "script exited with status 1", lastTransitionTime: now},
		}
		status := buildSystemInfoStatus(sources)
		entry := status.Statuses.CustomInfo["fail"]
		require.NotNil(entry.Message)
		require.Equal("script exited with status 1", *entry.Message)
	})

	t.Run("When a source succeeds it should have no message", func(t *testing.T) {
		sources := []cachedSource{
			{name: "ok", category: sourceCategoryBuiltIn, attempted: true, lastTransitionTime: now},
		}
		status := buildSystemInfoStatus(sources)
		entry := status.Statuses.SystemInfo["ok"]
		require.Nil(entry.Message)
	})
}

func Test_reconcileSources(t *testing.T) {
	require := require.New(t)
	now := time.Now()

	t.Run("When new keys added it should create fresh sources", func(t *testing.T) {
		result := reconcileSources(nil, []string{"cpuCores", "kernel"}, []string{"myScript"})
		require.Len(result, 3)
		require.Equal("cpuCores", result[0].name)
		require.Equal(sourceCategoryBuiltIn, result[0].category)
		require.False(result[0].attempted)
		require.Equal("myScript", result[2].name)
		require.Equal(sourceCategoryCustom, result[2].category)
	})

	t.Run("When existing keys preserved it should retain state", func(t *testing.T) {
		existing := []cachedSource{
			{name: "cpuCores", category: sourceCategoryBuiltIn, attempted: true, value: "4", hasValue: true, lastTransitionTime: now},
		}
		result := reconcileSources(existing, []string{"cpuCores"}, nil)
		require.Len(result, 1)
		require.True(result[0].attempted)
		require.Equal("4", result[0].value)
		require.Equal(now, result[0].lastTransitionTime)
	})

	t.Run("When key removed it should drop from result", func(t *testing.T) {
		existing := []cachedSource{
			{name: "cpuCores", category: sourceCategoryBuiltIn, attempted: true},
			{name: "gone", category: sourceCategoryCustom, attempted: true},
		}
		result := reconcileSources(existing, []string{"cpuCores"}, nil)
		require.Len(result, 1)
		require.Equal("cpuCores", result[0].name)
	})

	t.Run("When same name in both categories it should keep both", func(t *testing.T) {
		existing := []cachedSource{
			{name: "kernel", category: sourceCategoryBuiltIn, attempted: true, value: "5.10"},
			{name: "kernel", category: sourceCategoryCustom, attempted: true, value: "custom"},
		}
		result := reconcileSources(existing, []string{"kernel"}, []string{"kernel"})
		require.Len(result, 2)
		require.Equal(sourceCategoryBuiltIn, result[0].category)
		require.Equal(sourceCategoryCustom, result[1].category)
	})
}

func Test_classifyCustomError(t *testing.T) {
	require := require.New(t)

	t.Run("When context deadline exceeded it should report deadline", func(t *testing.T) {
		msg := classifyCustomError(context.DeadlineExceeded)
		require.Equal("collection exceeded deadline", msg)
	})

	t.Run("When context canceled it should report deadline", func(t *testing.T) {
		msg := classifyCustomError(context.Canceled)
		require.Equal("collection exceeded deadline", msg)
	})

	t.Run("When exit code present it should extract it", func(t *testing.T) {
		err := fmt.Errorf("code: 42: some stderr output")
		msg := classifyCustomError(err)
		require.Equal("script exited with status 42", msg)
	})

	t.Run("When no exit code it should default to status 1", func(t *testing.T) {
		err := fmt.Errorf("unknown error")
		msg := classifyCustomError(err)
		require.Equal("script exited with status 1", msg)
	})
}

func Test_computeSummaryStatus(t *testing.T) {
	require := require.New(t)
	now := time.Now()

	tests := []struct {
		name     string
		sources  []cachedSource
		expected v1beta1.SystemInfoSummaryStatusType
	}{
		{"When no sources it should be Unknown", nil, v1beta1.SystemInfoSummaryStatusUnknown},
		{"When all healthy it should be Healthy", []cachedSource{
			{attempted: true, lastTransitionTime: now},
			{attempted: true, lastTransitionTime: now},
		}, v1beta1.SystemInfoSummaryStatusHealthy},
		{"When all failed it should be Error", []cachedSource{
			{attempted: true, failed: true, lastTransitionTime: now},
			{attempted: true, failed: true, lastTransitionTime: now},
		}, v1beta1.SystemInfoSummaryStatusError},
		{"When mixed it should be Degraded", []cachedSource{
			{attempted: true, lastTransitionTime: now},
			{attempted: true, failed: true, lastTransitionTime: now},
		}, v1beta1.SystemInfoSummaryStatusDegraded},
		{"When any unattempted it should be Unknown", []cachedSource{
			{attempted: true},
			{attempted: false},
		}, v1beta1.SystemInfoSummaryStatusUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(tt.expected, computeSummaryStatus(tt.sources))
		})
	}
}

func Test_buildSystemInfoStatus_staleCleanup(t *testing.T) {
	require := require.New(t)
	now := time.Now()

	t.Run("When a script is removed its status entry should disappear", func(t *testing.T) {
		sources := []cachedSource{
			{name: "scriptA", category: sourceCategoryCustom, attempted: true, hasValue: true, lastTransitionTime: now},
			{name: "scriptB", category: sourceCategoryCustom, attempted: true, hasValue: true, lastTransitionTime: now},
		}
		status1 := buildSystemInfoStatus(sources)
		require.Len(status1.Statuses.CustomInfo, 2)

		// scriptB removed
		sources2 := reconcileSources(sources, nil, []string{"scriptA"})
		status2 := buildSystemInfoStatus(sources2)
		require.Len(status2.Statuses.CustomInfo, 1)
		_, hasA := status2.Statuses.CustomInfo["scriptA"]
		_, hasB := status2.Statuses.CustomInfo["scriptB"]
		require.True(hasA)
		require.False(hasB)
	})
}

package kubeflowmodelregistrysource

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	maxDNSSubdomainLength = 253
	maxDNSLabelLength     = 63
)

// normalizeName maps a free-form RegisteredModel.name to a valid
// RFC-1123 DNS subdomain (metadata.name).
//
// The algorithm (applied in order):
//  1. Unicode simple case fold to lowercase; any rune outside [a-z0-9.-] is
//     treated as a separator (non-ASCII → separator, not transliterated).
//  2. Replace every maximal run of separator/disallowed characters with a
//     single '-'.
//  3. Split on '.', drop empty labels, strip leading/trailing '-' from each
//     label; rejoin with '.'.
//  4. Per-label length: truncate any label > 63 bytes to 63; strip trailing
//     '-' if the truncation created one.
//  5. Total length: truncate the joined result to 253 bytes; strip trailing
//     '.' or '-' if truncation created one; re-apply step 4 to the final label.
//  6. Empty result → error.
//
// The original name is preserved verbatim in spec.displayName; this function
// only produces the DNS-safe metadata.name.
func normalizeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is empty")
	}

	// Step 1: lowercase + mark disallowed runes as separator.
	// We use a rune buffer and a single pass.
	var buf strings.Builder
	buf.Grow(len(name))

	for _, r := range name {
		folded := unicode.ToLower(r)
		if (folded >= 'a' && folded <= 'z') || (folded >= '0' && folded <= '9') {
			buf.WriteRune(folded)
		} else if folded == '.' {
			buf.WriteByte('.')
		} else {
			// Everything else (including non-ASCII runes) is a separator.
			buf.WriteByte('-')
		}
	}
	s := buf.String()

	// Step 2: collapse maximal runs of '-' into a single '-'.
	// Also collapse runs that contain '.' together as '.'. We do a character-by-character pass:
	// treat '-' as separator, '.' as dot, and collapse consecutive separators/dots into
	// a single '-' (or '.' if a dot appears in the run) by keeping only transitions.
	s = collapseRuns(s)

	// Step 3: split on '.', process labels, rejoin.
	labels := strings.Split(s, ".")
	var validLabels []string
	for _, label := range labels {
		label = strings.Trim(label, "-")
		if label == "" {
			continue
		}
		validLabels = append(validLabels, label)
	}
	if len(validLabels) == 0 {
		return "", fmt.Errorf("name %q normalizes to empty (no alphanumeric content)", name)
	}

	// Step 4: per-label length cap.
	for i, label := range validLabels {
		validLabels[i] = truncateLabel(label)
	}

	// Step 5: total length cap.
	result := strings.Join(validLabels, ".")
	if len(result) > maxDNSSubdomainLength {
		result = truncateSubdomain(result)
	}

	// Step 6: final validation.
	if result == "" {
		return "", fmt.Errorf("name %q normalizes to empty", name)
	}
	return result, nil
}

// collapseRuns replaces every maximal run of '-' (separator) with a single '-'.
// A '.' is kept as a '.' separator but if a run mixes '-' and '.' the whole run
// becomes '-' (since '.' inside a label is a separator, not a label boundary).
// Actually: we need to be careful. After step 1, the string contains only
// [a-z0-9.-]. The '.' character in the original name has special meaning
// (label boundary). Other separator chars (originally '-' or non-ASCII mapped
// to '-') collapse. So we collapse runs of '-' and keep '.' as-is.
func collapseRuns(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	inRun := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '-' {
			if !inRun {
				buf.WriteByte('-')
				inRun = true
			}
		} else {
			inRun = false
			buf.WriteByte(ch)
		}
	}
	return buf.String()
}

// truncateLabel truncates a single DNS label to maxDNSLabelLength bytes,
// stripping any trailing '-' created by the truncation.
func truncateLabel(label string) string {
	if len(label) <= maxDNSLabelLength {
		return label
	}
	label = label[:maxDNSLabelLength]
	label = strings.TrimRight(label, "-")
	return label
}

// truncateSubdomain truncates the full subdomain to maxDNSSubdomainLength bytes,
// strips any trailing '.' or '-', then re-applies the per-label cap to the
// now-truncated final label.
func truncateSubdomain(s string) string {
	s = s[:maxDNSSubdomainLength]
	s = strings.TrimRight(s, "-.")

	// The final label may have been truncated mid-label; re-apply per-label cap.
	dot := strings.LastIndexByte(s, '.')
	if dot < 0 {
		// Only one label.
		s = truncateLabel(s)
	} else {
		prefix := s[:dot+1]
		last := truncateLabel(s[dot+1:])
		if last == "" {
			// The last label became empty; drop the trailing dot.
			s = strings.TrimRight(prefix, ".")
		} else {
			s = prefix + last
		}
	}
	return s
}

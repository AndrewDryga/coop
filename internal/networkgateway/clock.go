package networkgateway

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// BootInstant is an absolute nanosecond position on the qualified namespace's
// CLOCK_BOOTTIME. It is NOT a Unix timestamp or a Go context deadline. Zero is
// invalid authority. Wall time is reserved for displayed observation timestamps.
type BootInstant int64

func (b BootInstant) Valid() bool                         { return b > 0 }
func (b BootInstant) Before(other BootInstant) bool       { return b.Valid() && b < other }
func (b BootInstant) After(other BootInstant) bool        { return b.Valid() && b > other }
func (b BootInstant) Equal(other BootInstant) bool        { return b == other }
func (b BootInstant) Sub(other BootInstant) time.Duration { return time.Duration(b - other) }
func (b BootInstant) Add(delta time.Duration) BootInstant {
	if !b.Valid() || delta > 0 && int64(b) > (1<<63-1)-int64(delta) {
		return 0
	}
	result := b + BootInstant(delta)
	if !result.Valid() {
		return 0
	}
	return result
}
func (b BootInstant) String() string               { return strconv.FormatInt(int64(b), 10) }
func (b BootInstant) MarshalJSON() ([]byte, error) { return json.Marshal(b.String()) }
func (b *BootInstant) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return Failure("clock_deadline_invalid")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 || strconv.FormatInt(parsed, 10) != value {
		return Failure("clock_deadline_invalid")
	}
	*b = BootInstant(parsed)
	return nil
}

type ClockDomain struct {
	BootID        string `json:"boot_id"`
	TimeNamespace string `json:"time_namespace"`
}

func (d ClockDomain) Valid() bool {
	parts := strings.Split(d.BootID, "-")
	if len(parts) != 5 {
		return false
	}
	for i, size := range []int{8, 4, 4, 4, 12} {
		if !lowerHex(parts[i], size) {
			return false
		}
	}
	n, err := strconv.ParseUint(d.TimeNamespace, 10, 64)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == d.TimeNamespace
}

// BootClock's immutable domain binds the controller, guard and its future child
// to the same boot/time namespace. A shared network namespace alone cannot do so.
type BootClock struct {
	domain ClockDomain
	read   func() (BootInstant, error)
}

func (c *BootClock) Domain() ClockDomain {
	if c == nil {
		return ClockDomain{}
	}
	return c.domain
}
func (c *BootClock) Now() (BootInstant, error) {
	if c == nil || c.read == nil || !c.domain.Valid() {
		return 0, Failure("clock_unavailable")
	}
	value, err := c.read()
	if err != nil || !value.Valid() {
		return 0, Failure("clock_unavailable")
	}
	return value, nil
}

// Internal comparisons reject this invalid sentinel; there is no wall-clock
// fallback. Keeping one failure representation also makes fault injection exact.
func (c *BootClock) instant() BootInstant {
	value, err := c.Now()
	if err != nil {
		return 0
	}
	return value
}

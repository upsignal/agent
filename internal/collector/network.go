package collector

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/upsignal/agent/internal/telemetry"
)

// ignoredInterfaces are the fallback devices the kernel creates whenever a tunnel module is loaded.
// They never carry traffic.
var ignoredInterfaces = map[string]bool{
	"lo": true, "gre0": true, "gretap0": true, "erspan0": true, "ip_vti0": true, "ip6_vti0": true,
	"sit0": true, "tunl0": true, "ip6tnl0": true, "ip6gre0": true,
}

// includeInterface drops loopback, tunnel fallback devices, and per-container veth pairs, which churn on
// Docker hosts and would crowd real interfaces out of the cap. Container metrics are out of scope for v1.
func includeInterface(name string) bool {
	return name != "" && !ignoredInterfaces[name] && !strings.HasPrefix(name, "veth")
}

// parseNetDev reads per-interface counters from /proc/net/dev, excluding loopback and virtual noise.
// Only counters are read; addresses and MACs are never collected.
func parseNetDev(data []byte) ([]telemetry.NetworkInterface, error) {
	ifaces := []telemetry.NetworkInterface{}
	for line := range strings.Lines(string(data)) {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue // the two header lines have no colon after the interface column
		}
		name = strings.TrimSpace(name)
		if !includeInterface(name) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			return nil, fmt.Errorf("/proc/net/dev %s: want 16 fields, got %d", name, len(f))
		}
		var v [16]uint64
		for i := range v {
			n, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("/proc/net/dev %s: %w", name, err)
			}
			v[i] = n
		}
		// Receive: bytes packets errs drop fifo frame compressed multicast; transmit starts at index 8.
		ifaces = append(ifaces, telemetry.NetworkInterface{
			Name:    name,
			RxBytes: v[0], RxPackets: v[1], RxErrors: v[2], RxDropped: v[3],
			TxBytes: v[8], TxPackets: v[9], TxErrors: v[10], TxDropped: v[11],
		})
	}
	slices.SortFunc(ifaces, func(a, b telemetry.NetworkInterface) int { return strings.Compare(a.Name, b.Name) })
	return ifaces, nil
}

func (c *Collector) collectNetwork() ([]telemetry.NetworkInterface, error) {
	data, err := c.read("proc/net/dev")
	if err != nil {
		return nil, err
	}
	ifaces, err := parseNetDev(data)
	if err != nil {
		return nil, err
	}
	if len(ifaces) > MaxInterfaces {
		return ifaces[:MaxInterfaces], fmt.Errorf("reported first %d of %d interfaces", MaxInterfaces, len(ifaces))
	}
	return ifaces, nil
}

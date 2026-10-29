package config

import (
	"fmt"
	"strconv"
	"strings"
)

// MaxEditorOriginPorts bounds the pool so a typo like "1-65535" cannot make the
// daemon willing to bind most of the machine's ports.
const MaxEditorOriginPorts = 256

// ParseEditorOriginPorts expands network.editor_origin_ports ("9100-9119",
// "9100,9105-9107") into its ports, ascending and de-duplicated. The empty string
// is the valid "off" value and yields no ports. Ports below 1024 are refused: the
// pool is for unprivileged loopback listeners.
func ParseEditorOriginPorts(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	seen := map[int]bool{}
	var ports []int
	add := func(p int) error {
		if p < 1024 || p > 65535 {
			return fmt.Errorf("network.editor_origin_ports: port %d is outside 1024-65535", p)
		}
		if !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
		if len(ports) > MaxEditorOriginPorts {
			return fmt.Errorf("network.editor_origin_ports: more than %d ports; narrow the pool", MaxEditorOriginPorts)
		}
		return nil
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("network.editor_origin_ports: %q is not a port or range (use e.g. 9100-9119)", part)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil || b < a {
				return nil, fmt.Errorf("network.editor_origin_ports: %q is not a valid ascending range", part)
			}
		}
		if b-a >= MaxEditorOriginPorts {
			return nil, fmt.Errorf("network.editor_origin_ports: range %q exceeds %d ports; narrow the pool", part, MaxEditorOriginPorts)
		}
		for p := a; p <= b; p++ {
			if err := add(p); err != nil {
				return nil, err
			}
		}
	}
	sortInts(ports)
	return ports, nil
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// validateEditorOriginPorts is the settable-key validator for the pool.
func validateEditorOriginPorts(_, v string) error {
	_, err := ParseEditorOriginPorts(v)
	return err
}

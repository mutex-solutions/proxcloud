package pve

import (
	"net/url"
	"strconv"
)

// formVals is sugar for building url.Values without the double-slice noise.
// Empty values are dropped — PVE treats an empty param as "set to empty", which
// for most keys is invalid.
type formVals map[string]string

func (f formVals) values() url.Values {
	v := url.Values{}
	for k, s := range f {
		if s != "" {
			v.Set(k, s)
		}
	}
	return v
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// pbool renders a Proxmox boolean (1/0). Only emits "1" — omit for false/default.
func pbool(b bool) string {
	if b {
		return "1"
	}
	return ""
}

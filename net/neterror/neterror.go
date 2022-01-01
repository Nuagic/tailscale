// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package neterror classifies network errors.
package neterror

import (
	"errors"
	"runtime"
	"syscall"
)

var errEPERM error = syscall.EPERM // box it into interface just once

// TreatAsLostUDP reports whether err is an error from a UDP send
// operation that should be treated as a UDP packet that just got
// lost.
//
// Notably, on Linux this reports true for EPERM errors (from outbound
// firewall blocks) which aren't really send errors; they're just
// sends that are never going to make it because the local OS blocked
// it.
func TreatAsLostUDP(err error) bool {
	if err == nil {
		return false
	}
	switch runtime.GOOS {
	case "linux":
		// Linux, while not documented in the man page,
		// returns EPERM when there's an OUTPUT rule with -j
		// DROP or -j REJECT.  We use this very specific
		// Linux+EPERM check rather than something super broad
		// like net.Error.Temporary which could be anything.
		//
		// For now we only do this on Linux, as such outgoing
		// firewall violations mapping to syscall errors
		// hasn't yet been observed on other OSes.
		return errors.Is(err, errEPERM)
	}
	return false
}

// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build dragonfly netbsd

package ipv4

import (
	"net"
	"syscall"
)

var (
	ctlOpts = [ctlMax]ctlOpt{
		ctlTTL:       {sysIP_RECVTTL, 1, marshalTTL, parseTTL},
		ctlDst:       {sysIP_RECVDSTADDR, net.IPv4len, marshalDst, parseDst},
		ctlInterface: {sysIP_RECVIF, syscall.SizeofSockaddrDatalink, marshalInterface, parseInterface},
	}

	sockOpts = [ssoMax]sockOpt{
		ssoTOS:                {sysIP_TOS, ssoTypeInt},
		ssoTTL:                {sysIP_TTL, ssoTypeInt},
		ssoMulticastTTL:       {sysIP_MULTICAST_TTL, ssoTypeByte},
		ssoMulticastInterface: {sysIP_MULTICAST_IF, ssoTypeInterface},
		ssoMulticastLoopback:  {sysIP_MULTICAST_LOOP, ssoTypeInt},
		ssoReceiveTTL:         {sysIP_RECVTTL, ssoTypeInt},
		ssoReceiveDst:         {sysIP_RECVDSTADDR, ssoTypeInt},
		ssoReceiveInterface:   {sysIP_RECVIF, ssoTypeInt},
		ssoHeaderPrepend:      {sysIP_HDRINCL, ssoTypeInt},
		ssoJoinGroup:          {sysIP_ADD_MEMBERSHIP, ssoTypeIPMreq},
		ssoLeaveGroup:         {sysIP_DROP_MEMBERSHIP, ssoTypeIPMreq},
	}
)

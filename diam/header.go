// Copyright 2013 Alexandre Fiori
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Diameter message header parser and helpers.

package diam

import (
	"encoding/binary"
	"fmt"
	"io"
	"unsafe"
)

type Header struct {
	Version          uint8
	RawMessageLength [3]uint8
	CommandFlags     uint8
	RawCommandCode   [3]uint8
	ApplicationId    uint32
	HopByHopId       uint32
	EndToEndId       uint32
}

// ReadHeader reads one diameter header from the connection and return it.
func ReadHeader(r io.Reader) (*Header, error) {
	hdr := new(Header)
	if err := binary.Read(r, binary.BigEndian, hdr); err != nil {
		return nil, err
	}
	if hdr.Version != byte(1) {
		return nil,
			fmt.Errorf("Invalid diameter version %d", hdr.Version)
	}
	return hdr, nil
}

// String returns the diameter header in human readable format.
func (hdr *Header) String() string {
	rflag := hdr.CommandFlags&0x80 > 0
	pflag := hdr.CommandFlags&0x40 > 0
	eflag := hdr.CommandFlags&0x20 > 0
	tflag := hdr.CommandFlags&0x10 > 0
	cmd := hdr.CommandName()
	return fmt.Sprintf(
		"%s (%s) Header{Code=%d,Version=%d,"+
			"MessageLength=%d,CommandFlags={r=%v,p=%v,e=%v,t=%v},"+
			"ApplicationId=%d,HopByHopId=%#v,EndToEndId=%#v}",
		cmd.Name, cmd.Abbrev, hdr.CommandCode(), hdr.Version,
		hdr.MessageLength(), rflag, pflag, eflag, tflag,
		hdr.ApplicationId, hdr.HopByHopId, hdr.EndToEndId)
}

// MessageLength is a helper function returns the RawMessageLength as int.
func (hdr *Header) MessageLength() uint32 {
	return uint24To32(hdr.RawMessageLength)
}

// UpdateLength is a helper function that updates RawMessageLength.
func (hdr *Header) SetMessageLength(length uint32) {
	hdr.RawMessageLength = uint32To24(uint32(unsafe.Sizeof(Header{})) + length)
}

// CommandCode is a helper function that returns the RawCommandCode as int.
func (hdr *Header) CommandCode() uint32 {
	return uint24To32(hdr.RawCommandCode)
}

// CommandName is a helper function that returns the name of the command based
// on its code.
func (hdr *Header) CommandName() *Command {
	var nameSuffix, abbrevSuffix string
	if hdr.CommandFlags&0x80 > 0 {
		nameSuffix = "-Request"
		abbrevSuffix = "R"
	} else {
		nameSuffix = "-Answer"
		abbrevSuffix = "A"
	}
	code := hdr.CommandCode()
	var resp Command
	if cmd, ok := commandCodes[code]; ok {
		resp.Name = cmd.Name + nameSuffix
		resp.Abbrev = cmd.Abbrev + abbrevSuffix
	} else {
		resp.Name = "Unknown"
		resp.Abbrev = "?"
	}
	return &resp
}

type Command struct {
	Name   string
	Abbrev string
}

// TODO: Allow applications to register their commands?
var commandCodes = map[uint32]Command{
	274: {"Abbort-Session", "AS"},
	271: {"Accounting", "AC"},
	257: {"Capabilities-Exchange", "CE"},
	280: {"Device-Watchdog", "DW"},
	282: {"Disconnect-Peer", "DP"},
	258: {"Re-Auth", "RA"},
	275: {"Session-Termination", "ST"},
}

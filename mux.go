// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssh

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// chanList is a thread safe channel list.
type chanList struct {
	// protects concurrent access to chans
	sync.Mutex

	// chans are indexed by the local id of the channel, which the
	// other side should send in the PeersId field.
	chans []*channel

	// This is a debugging aid: it offsets all IDs by this
	// amount. This helps distinguish otherwise identical
	// server/client muxes
	offset uint32
}

// Assigns a channel ID to the given channel.
func (c *chanList) add(ch *channel) uint32 {
	c.Lock()
	defer c.Unlock()
	for i := range c.chans {
		if c.chans[i] == nil {
			c.chans[i] = ch
			return uint32(i) + c.offset
		}
	}
	c.chans = append(c.chans, ch)
	return uint32(len(c.chans)-1) + c.offset
}

// getChan returns the channel for the given ID.
func (c *chanList) getChan(id uint32) *channel {
	id -= c.offset

	c.Lock()
	defer c.Unlock()
	if id < uint32(len(c.chans)) {
		return c.chans[id]
	}
	return nil
}

func (c *chanList) remove(id uint32) {
	id -= c.offset
	c.Lock()
	if id < uint32(len(c.chans)) {
		c.chans[id] = nil
	}
	c.Unlock()
}

// dropAll forgets all channels it knows, returning them in a slice.
func (c *chanList) dropAll() []*channel {
	c.Lock()
	defer c.Unlock()
	var r []*channel

	for _, ch := range c.chans {
		if ch == nil {
			continue
		}
		r = append(r, ch)
	}
	c.chans = nil
	return r
}

// mux represents the state for the SSH connection protocol, which
// multiplexes many channels onto a single packet transport.
type mux struct {
	conn     packetConn
	chanList chanList

	incomingChannels chan NewChannel

	globalSentMu     sync.Mutex
	globalResponses  chan interface{}
	incomingRequests chan *Request

	errCond *sync.Cond
	err     error

	// debugMux, if set, causes messages in the connection protocol to be
	// logged.
	debugMux bool
	logger   *slog.Logger
}

// When debugging, each new chanList instantiation has a different
// offset.
var globalOff uint32

func (m *mux) Wait() error {
	m.errCond.L.Lock()
	defer m.errCond.L.Unlock()
	for m.err == nil {
		m.errCond.Wait()
	}
	return m.err
}

// newMux returns a mux that runs over the given connection.
func newMux(p packetConn, debugMux bool) *mux {
	m := &mux{
		conn:             p,
		incomingChannels: make(chan NewChannel, chanSize),
		globalResponses:  make(chan interface{}, 1),
		incomingRequests: make(chan *Request, chanSize),
		errCond:          newCond(),
	}

	if debugMux {
		m.debugMux = true
		m.chanList.offset = atomic.AddUint32(&globalOff, 1)
	}

	go m.loop()
	return m
}

func (m *mux) EnableDebug() {
	if !m.debugMux {
		m.debugMux = true
		m.chanList.offset = atomic.AddUint32(&globalOff, 1)
	}
}

func msgString(offset uint32, msg any) string {
	description := ""
	t := ""
	switch v := msg.(type) {
	case globalRequestMsg:
		t = "globalRequestMsg"
		description = fmt.Sprintf("Type: %s WantReply: %v DataLen: %d", v.Type, v.WantReply, len(v.Data))
	case globalRequestSuccessMsg:
		t = "globalRequestSuccessMsg"
		description = fmt.Sprintf("DataLen: %d", len(v.Data))
	case globalRequestFailureMsg:
		t = "globalRequestFailureMsg"
		description = fmt.Sprintf("DataLen: %d", len(v.Data))
	case pongMsg:
		t = "pongMsg"
		description = fmt.Sprintf("DataLen: %d", len(v.Data))
	case channelOpenFailureMsg:
		t = "channelOpenFailureMsg"
		description = fmt.Sprintf(
			"Reason: '%s' Message: '%s' Language: %s PeersID: %d",
			v.Reason.String(),
			v.Message,
			v.Language,
			v.PeersID,
		)
	case channelOpenMsg:
		t = "channelOpenMsg"
		description = fmt.Sprintf(
			"ChanType: %s DataLen: %d PeersWindow: %d MaxPacketSize: %d PeersID: %d",
			v.ChanType,
			len(v.TypeSpecificData),
			v.PeersWindow,
			v.MaxPacketSize,
			v.PeersID,
		)
	case channelRequestFailureMsg:
		t = "channelRequestFailureMsg"
		description = fmt.Sprintf("PeersID: %d", v.PeersID)
	default:
		t = "unknown"
		description = fmt.Sprintf("RawMessage: %#v", msg)
	}

	return fmt.Sprintf("%s (offset %d): %s", t, offset, description)
}

func (m *mux) sendMessage(msg interface{}) error {
	p := Marshal(msg)
	if m.debugEnabled() {
		// do not use m.debug() msgString can huge for execute
		m.logger.Debug(msgString(m.chanList.offset, msg))
	}
	return m.conn.writePacket(p)
}

func (m *mux) SendRequest(name string, wantReply bool, payload []byte) (bool, []byte, error) {
	if wantReply {
		m.globalSentMu.Lock()
		defer m.globalSentMu.Unlock()
	}

	if err := m.sendMessage(globalRequestMsg{
		Type:      name,
		WantReply: wantReply,
		Data:      payload,
	}); err != nil {
		return false, nil, err
	}

	if !wantReply {
		return false, nil, nil
	}

	msg, ok := <-m.globalResponses
	if !ok {
		return false, nil, io.EOF
	}
	switch msg := msg.(type) {
	case *globalRequestFailureMsg:
		return false, msg.Data, nil
	case *globalRequestSuccessMsg:
		return true, msg.Data, nil
	default:
		return false, nil, fmt.Errorf("ssh: unexpected response to request: %#v", msg)
	}
}

// ackRequest must be called after processing a global request that
// has WantReply set.
func (m *mux) ackRequest(ok bool, data []byte) error {
	if ok {
		return m.sendMessage(globalRequestSuccessMsg{Data: data})
	}
	return m.sendMessage(globalRequestFailureMsg{Data: data})
}

func (m *mux) Close() error {
	return m.conn.Close()
}

// loop runs the connection machine. It will process packets until an
// error is encountered. To synchronize on loop exit, use mux.Wait.
func (m *mux) loop() {
	m.debug("Starting mux loop...")

	var err error
	for err == nil {
		err = m.onePacket()
	}

	for _, ch := range m.chanList.dropAll() {
		ch.close()
	}

	close(m.incomingChannels)
	close(m.incomingRequests)
	close(m.globalResponses)

	m.conn.Close()

	m.errCond.L.Lock()
	m.err = err
	m.errCond.Broadcast()
	m.errCond.L.Unlock()

	m.debug("mux loop stopped: %v", err)
}

// onePacket reads and processes one packet.
func (m *mux) onePacket() error {
	packet, err := m.conn.readPacket()
	if err != nil {
		return err
	}

	if m.debugEnabled() {
		if packet[0] == msgChannelData || packet[0] == msgChannelExtendedData {
			m.logger.Debug(fmt.Sprintf("decoding(%d): data packet - %d bytes", m.chanList.offset, len(packet)))
		} else {
			p, _ := decode(packet)
			m.logger.Debug(fmt.Sprintf("decoding(%d): %d %#v - %d bytes", m.chanList.offset, packet[0], p, len(packet)))
		}
	}

	switch packet[0] {
	case msgChannelOpen:
		return m.handleChannelOpen(packet)
	case msgGlobalRequest, msgRequestSuccess, msgRequestFailure:
		return m.handleGlobalPacket(packet)
	case msgPing:
		var msg pingMsg
		if err := Unmarshal(packet, &msg); err != nil {
			return fmt.Errorf("failed to unmarshal ping@openssh.com message: %w", err)
		}
		return m.sendMessage(pongMsg(msg))
	}

	// assume a channel packet.
	if len(packet) < 5 {
		return parseError(packet[0])
	}
	id := binary.BigEndian.Uint32(packet[1:])
	ch := m.chanList.getChan(id)
	if ch == nil {
		return m.handleUnknownChannelPacket(id, packet)
	}

	return ch.handlePacket(packet)
}

func (m *mux) handleGlobalPacket(packet []byte) error {
	msg, err := decode(packet)
	if err != nil {
		return err
	}

	switch msg := msg.(type) {
	case *globalRequestMsg:
		m.incomingRequests <- &Request{
			Type:      msg.Type,
			WantReply: msg.WantReply,
			Payload:   msg.Data,
			mux:       m,
		}
	case *globalRequestSuccessMsg, *globalRequestFailureMsg:
		m.globalResponses <- msg
	default:
		panic(fmt.Sprintf("not a global message %#v", msg))
	}

	return nil
}

// handleChannelOpen schedules a channel to be Accept()ed.
func (m *mux) handleChannelOpen(packet []byte) error {
	var msg channelOpenMsg
	if err := Unmarshal(packet, &msg); err != nil {
		return err
	}

	if msg.MaxPacketSize < minPacketLength || msg.MaxPacketSize > 1<<31 {
		failMsg := channelOpenFailureMsg{
			PeersID:  msg.PeersID,
			Reason:   ConnectionFailed,
			Message:  "invalid request",
			Language: "en_US.UTF-8",
		}
		return m.sendMessage(failMsg)
	}

	c := m.newChannel(msg.ChanType, channelInbound, msg.TypeSpecificData)
	c.remoteId = msg.PeersID
	c.maxRemotePayload = msg.MaxPacketSize
	c.remoteWin.add(msg.PeersWindow)
	m.incomingChannels <- c
	return nil
}

func (m *mux) OpenChannel(chanType string, extra []byte) (Channel, <-chan *Request, error) {
	ch, err := m.openChannel(chanType, extra)
	if err != nil {
		return nil, nil, err
	}

	return ch, ch.incomingRequests, nil
}

func (m *mux) openChannel(chanType string, extra []byte) (*channel, error) {
	ch := m.newChannel(chanType, channelOutbound, extra)

	ch.maxIncomingPayload = channelMaxPacket

	open := channelOpenMsg{
		ChanType:         chanType,
		PeersWindow:      ch.myWindow,
		MaxPacketSize:    ch.maxIncomingPayload,
		TypeSpecificData: extra,
		PeersID:          ch.localId,
	}
	if err := m.sendMessage(open); err != nil {
		return nil, err
	}

	switch msg := (<-ch.msg).(type) {
	case *channelOpenConfirmMsg:
		return ch, nil
	case *channelOpenFailureMsg:
		return nil, &OpenChannelError{msg.Reason, msg.Message}
	default:
		return nil, fmt.Errorf("ssh: unexpected packet in response to channel open: %T", msg)
	}
}

func (m *mux) handleUnknownChannelPacket(id uint32, packet []byte) error {
	msg, err := decode(packet)
	if err != nil {
		return err
	}

	switch msg := msg.(type) {
	// RFC 4254 section 5.4 says unrecognized channel requests should
	// receive a failure response.
	case *channelRequestMsg:
		if msg.WantReply {
			return m.sendMessage(channelRequestFailureMsg{
				PeersID: msg.PeersID,
			})
		}
		return nil
	default:
		return fmt.Errorf("ssh: invalid channel %d", id)
	}
}

func (m *mux) debugEnabled() bool {
	return m.debugMux && m.logger != nil
}

func (m *mux) debug(format string, args ...any) {
	if !m.debugEnabled() {
		return
	}

	m.logger.Debug(fmt.Sprintf(format, args...))
}

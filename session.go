// Copyright 2015-2017 HenryLee. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tp

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/henrylee2cn/go-logging/color"
	"github.com/henrylee2cn/goutil"
	"github.com/henrylee2cn/goutil/errors"
	"github.com/json-iterator/go"

	"github.com/henrylee2cn/teleport/socket"
)

// Session a connection session.
type (
	Session interface {
		ChangeId(newId string)
		Close() error
		GoPull(uri string, args interface{}, reply interface{}, done chan *PullCmd, setting ...socket.PacketSetting)
		Id() string
		IsOk() bool
		Peer() *Peer
		Pull(uri string, args interface{}, reply interface{}, setting ...socket.PacketSetting) *PullCmd
		Push(uri string, args interface{}, setting ...socket.PacketSetting) error
		ReadTimeout() time.Duration
		RemoteIp() string
		SetReadTimeout(duration time.Duration)
		SetWriteTimeout(duration time.Duration)
		Socket() socket.Socket
		WriteTimeout() time.Duration
		Public() goutil.Map
		PublicLen() int
	}
	ForeSession interface {
		ChangeId(newId string)
		Close() error
		Id() string
		IsOk() bool
		Peer() *Peer
		RemoteIp() string
		SetReadTimeout(duration time.Duration)
		SetWriteTimeout(duration time.Duration)
		Public() goutil.Map
		PublicLen() int
		Send(packet *socket.Packet) error
		Receive(packet *socket.Packet) error
	}
	session struct {
		peer                  *Peer
		pullRouter            *Router
		pushRouter            *Router
		pushSeq               uint64
		pushSeqLock           sync.Mutex
		pullSeq               uint64
		pullCmdMap            goutil.Map
		pullSeqLock           sync.Mutex
		socket                socket.Socket
		closed                int32 // 0:false, 1:true
		disconnected          int32 // 0:false, 1:true
		closeLock             sync.RWMutex
		disconnectLock        sync.RWMutex
		writeLock             sync.Mutex
		graceCtxWaitGroup     sync.WaitGroup
		gracePullCmdWaitGroup sync.WaitGroup
		readTimeout           int64 // time.Duration
		writeTimeout          int64 // time.Duration
	}
)

var (
	_ Session     = new(session)
	_ ForeSession = new(session)
)

func newSession(peer *Peer, conn net.Conn, id ...string) *session {
	var s = &session{
		peer:         peer,
		pullRouter:   peer.PullRouter,
		pushRouter:   peer.PushRouter,
		socket:       socket.NewSocket(conn, id...),
		pullCmdMap:   goutil.RwMap(),
		readTimeout:  peer.defaultReadTimeout,
		writeTimeout: peer.defaultWriteTimeout,
	}
	return s
}

// Peer returns the peer.
func (s *session) Peer() *Peer {
	return s.peer
}

// Socket returns the Socket.
func (s *session) Socket() socket.Socket {
	return s.socket
}

// Id returns the session id.
func (s *session) Id() string {
	return s.socket.Id()
}

// ChangeId changes the session id.
func (s *session) ChangeId(newId string) {
	oldId := s.Id()
	s.socket.ChangeId(newId)
	s.peer.sessHub.Set(s)
	s.peer.sessHub.Delete(oldId)
	Tracef("session changes id: %s -> %s", oldId, newId)
}

// RemoteIp returns the remote peer ip.
func (s *session) RemoteIp() string {
	return s.socket.RemoteAddr().String()
}

// ReadTimeout returns readdeadline for underlying net.Conn.
func (s *session) ReadTimeout() time.Duration {
	return time.Duration(atomic.LoadInt64(&s.readTimeout))
}

// WriteTimeout returns writedeadline for underlying net.Conn.
func (s *session) WriteTimeout() time.Duration {
	return time.Duration(atomic.LoadInt64(&s.writeTimeout))
}

// ReadTimeout returns readdeadline for underlying net.Conn.
func (s *session) SetReadTimeout(duration time.Duration) {
	atomic.StoreInt64(&s.readTimeout, int64(duration))
}

// WriteTimeout returns writedeadline for underlying net.Conn.
func (s *session) SetWriteTimeout(duration time.Duration) {
	atomic.StoreInt64(&s.writeTimeout, int64(duration))
}

// Send sends packet to peer.
func (s *session) Send(packet *socket.Packet) error {
	return s.socket.WritePacket(packet)
}

// Receive receives a packet from peer.
func (s *session) Receive(packet *socket.Packet) error {
	return s.socket.ReadPacket(packet)
}

// GoPull sends a packet and receives reply asynchronously.
// If the args is []byte or *[]byte type, it can automatically fill in the body codec name.
func (s *session) GoPull(uri string, args interface{}, reply interface{}, done chan *PullCmd, setting ...socket.PacketSetting) {
	if done == nil && cap(done) == 0 {
		// It must arrange that done has enough buffer for the number of simultaneous
		// RPCs that will be using that channel. If the channel
		// is totally unbuffered, it's best not to run at all.
		Panicf("*session.GoPull(): done channel is unbuffered")
	}
	s.pullSeqLock.Lock()
	seq := s.pullSeq
	s.pullSeq++
	s.pullSeqLock.Unlock()
	output := &socket.Packet{
		Header: &socket.Header{
			Seq:  seq,
			Type: TypePull,
			Uri:  uri,
			Gzip: s.peer.defaultBodyGzipLevel,
		},
		Body:        args,
		HeaderCodec: s.peer.defaultHeaderCodec,
	}
	for _, f := range setting {
		f(output)
	}
	if len(output.BodyCodec) == 0 {
		switch body := args.(type) {
		case []byte:
			output.BodyCodec = socket.GetCodecNameFromBytes(body)
		case *[]byte:
			output.BodyCodec = socket.GetCodecNameFromBytes(*body)
		default:
			output.BodyCodec = s.peer.defaultBodyCodec
		}
	}

	cmd := &PullCmd{
		sess:     s,
		output:   output,
		reply:    reply,
		doneChan: done,
		start:    time.Now(),
		public:   goutil.RwMap(),
	}

	{
		// count pull-launch
		s.gracePullCmdWaitGroup.Add(1)
	}

	if s.socket.PublicLen() > 0 {
		s.socket.Public().Range(func(key, value interface{}) bool {
			cmd.public.Store(key, value)
			return true
		})
	}

	defer func() {
		if p := recover(); p != nil {
			Errorf("panic:\n%v\n%s", p, goutil.PanicTrace(1))
		}
	}()

	s.pullCmdMap.Store(output.Header.Seq, cmd)
	err := s.peer.pluginContainer.PreWritePull(cmd)
	if err == nil {
		if err = s.write(output); err == nil {
			if err = s.peer.pluginContainer.PostWritePull(cmd); err != nil {
				Errorf("%s", err.Error())
			}
			return
		}
	}
	cmd.Xerror = NewXerror(StatusWriteFailed, err.Error())
	cmd.done()
}

// Pull sends a packet and receives reply.
// If the args is []byte or *[]byte type, it can automatically fill in the body codec name.
func (s *session) Pull(uri string, args interface{}, reply interface{}, setting ...socket.PacketSetting) *PullCmd {
	doneChan := make(chan *PullCmd, 1)
	s.GoPull(uri, args, reply, doneChan, setting...)
	pullCmd := <-doneChan
	defer func() {
		recover()
	}()
	close(doneChan)
	return pullCmd
}

// Push sends a packet, but do not receives reply.
// If the args is []byte or *[]byte type, it can automatically fill in the body codec name.
func (s *session) Push(uri string, args interface{}, setting ...socket.PacketSetting) (err error) {
	start := time.Now()

	s.pushSeqLock.Lock()
	ctx := s.peer.getContext(s, true)
	output := ctx.output
	header := output.Header
	header.Seq = s.pushSeq
	s.pushSeq++
	s.pushSeqLock.Unlock()

	ctx.start = start

	header.Type = TypePush
	header.Uri = uri
	header.Gzip = s.peer.defaultBodyGzipLevel

	output.Body = args
	output.HeaderCodec = s.peer.defaultHeaderCodec

	for _, f := range setting {
		f(output)
	}
	if len(output.BodyCodec) == 0 {
		switch body := args.(type) {
		case nil:
		case []byte:
			output.BodyCodec = socket.GetCodecNameFromBytes(body)
		case *[]byte:
			output.BodyCodec = socket.GetCodecNameFromBytes(*body)
		default:
			output.BodyCodec = s.peer.defaultBodyCodec
		}
	}

	defer func() {
		if p := recover(); p != nil {
			err = errors.Errorf("panic:\n%v\n%s", p, goutil.PanicTrace(1))
		}
		s.runlog(time.Since(ctx.start), nil, output)
		s.peer.putContext(ctx, true)
	}()

	err = s.peer.pluginContainer.PreWritePush(ctx)
	if err == nil {
		if err = s.write(output); err == nil {
			err = s.peer.pluginContainer.PostWritePush(ctx)
		}
	}
	return err
}

// Public returns temporary public data of session(socket).
func (s *session) Public() goutil.Map {
	return s.socket.Public()
}

// PublicLen returns the length of public data of session(socket).
func (s *session) PublicLen() int {
	return s.socket.PublicLen()
}

func (s *session) startReadAndHandle() {
	defer func() {
		if p := recover(); p != nil {
			Debugf("panic:\n%v\n%s", p, goutil.PanicTrace(2))
		}
		atomic.StoreInt32(&s.disconnected, 1)
		s.Close()
	}()

	var (
		err         error
		readTimeout time.Duration
	)

	// read pull, pull reple or push
	for s.IsOk() {
		var ctx = s.peer.getContext(s, false)
		err = s.peer.pluginContainer.PreReadHeader(ctx)
		if err != nil {
			s.peer.putContext(ctx, false)
			s.readDisconnected(err)
			return
		}

		if readTimeout = s.ReadTimeout(); readTimeout > 0 {
			s.socket.SetReadDeadline(time.Now().Add(readTimeout))
		}

		err = s.socket.ReadPacket(ctx.input)
		if err != nil {
			s.peer.putContext(ctx, false)
			s.readDisconnected(err)
			return
		}
		if !s.IsOk() {
			s.peer.putContext(ctx, false)
			return
		}

		s.graceCtxWaitGroup.Add(1)
		if !Go(func() {
			defer func() {
				if p := recover(); p != nil {
					Debugf("panic:\n%v\n%s", p, goutil.PanicTrace(1))
				}
				s.peer.putContext(ctx, true)
			}()
			switch ctx.input.Header.Type {
			case TypeReply:
				// handles pull reply
				ctx.handleReply()

			case TypePush:
				//  handles push
				ctx.handlePush()

			case TypePull:
				// handles and replies pull
				ctx.handlePull()

			default:
				ctx.handleUnsupported()
			}
		}) {
			s.graceCtxWaitGroup.Done()
		}
	}
}

// ErrConnClosed connection is closed error.
var ErrConnClosed = errors.New("connection is closed")

func (s *session) write(packet *socket.Packet) (err error) {
	if s.isDisconnected() {
		return ErrConnClosed
	}
	var (
		writeTimeout = s.WriteTimeout()
		now          time.Time
	)
	if writeTimeout > 0 {
		now = time.Now()
	}

	s.writeLock.Lock()

	if s.isDisconnected() {
		s.writeLock.Unlock()
		return ErrConnClosed
	}

	defer func() {
		if p := recover(); p != nil {
			err = errors.Errorf("panic:\n%v\n%s", p, goutil.PanicTrace(2))
		} else if err == io.EOF || err == socket.ErrProactivelyCloseSocket {
			err = ErrConnClosed
		}
		s.writeLock.Unlock()
	}()

	if writeTimeout > 0 {
		s.socket.SetWriteDeadline(now.Add(writeTimeout))
	}
	err = s.socket.WritePacket(packet)
	return err
}

// IsOk checks if the session is ok.
func (s *session) IsOk() bool {
	return atomic.LoadInt32(&s.disconnected) != 1 && atomic.LoadInt32(&s.closed) != 1
}

// isDisconnected checks if the session is ok.
func (s *session) isDisconnected() bool {
	return atomic.LoadInt32(&s.disconnected) == 1
}

var (
	deadlineForEndlessRead = time.Time{}
	deadlineForStopRead    = time.Time{}.Add(1)
)

// Close closes the session.
func (s *session) Close() (err error) {
	if atomic.LoadInt32(&s.closed) == 1 {
		return nil
	}

	s.closeLock.Lock()
	if atomic.LoadInt32(&s.closed) == 1 {
		s.closeLock.Unlock()
		return nil
	}
	atomic.StoreInt32(&s.closed, 1)

	defer func() {
		recover()

		s.graceCtxWaitGroup.Wait()
		s.gracePullCmdWaitGroup.Wait()

		// make sure return s.startReadAndHandle
		s.socket.SetReadDeadline(deadlineForStopRead)

		err = s.socket.Close()

		s.closeLock.Unlock()
	}()

	s.peer.sessHub.Delete(s.Id())

	if !s.isDisconnected() {
		// make sure do not disconnect because of reading timeout.
		// wait for the subsequent write to complete.
		s.socket.SetReadDeadline(deadlineForEndlessRead)
	}

	return
}

func (s *session) readDisconnected(err error) {
	if atomic.LoadInt32(&s.closed) == 1 || atomic.LoadInt32(&s.disconnected) == 1 {
		return
	}
	s.disconnectLock.Lock()
	defer s.disconnectLock.Unlock()
	if atomic.LoadInt32(&s.closed) == 1 || atomic.LoadInt32(&s.disconnected) == 1 {
		return
	}

	atomic.StoreInt32(&s.disconnected, 1)

	if err != io.EOF && err != socket.ErrProactivelyCloseSocket {
		Debugf("disconnected when reading: %s", err.Error())
	}

	s.graceCtxWaitGroup.Wait()

	s.pullCmdMap.Range(func(_, v interface{}) bool {
		pullCmd := v.(*PullCmd)
		pullCmd.cancel()
		return true
	})
}

func isPushLaunch(input, output *socket.Packet) bool {
	return input == nil || (output != nil && output.Header.Type == TypePush)
}
func isPushHandle(input, output *socket.Packet) bool {
	return output == nil || (input != nil && input.Header.Type == TypePush)
}
func isPullLaunch(input, output *socket.Packet) bool {
	return output != nil && output.Header.Type == TypePull
}
func isPullHandle(input, output *socket.Packet) bool {
	return output != nil && output.Header.Type == TypeReply
}

func (s *session) runlog(costTime time.Duration, input, output *socket.Packet) {
	var (
		printFunc func(string, ...interface{})
		slowStr   string
		logformat string
		printBody = s.peer.printBody
	)
	if costTime < s.peer.slowCometDuration {
		printFunc = Infof
	} else {
		printFunc = Warnf
		slowStr = "(slow)"
	}

	if isPushLaunch(input, output) {
		if printBody {
			logformat = "[push-launch] remote-ip: %s | cost-time: %s%s | uri: %-30s |\nSEND:\n packet-length: %d\n body[-json]: %s\n"
			printFunc(logformat, s.RemoteIp(), costTime, slowStr, output.Header.Uri, output.Length, bodyLogBytes(output))

		} else {
			logformat = "[push-launch] remote-ip: %s | cost-time: %s%s | uri: %-30s |\nSEND:\n packet-length: %d\n"
			printFunc(logformat, s.RemoteIp(), costTime, slowStr, output.Header.Uri, output.Length)
		}

	} else if isPushHandle(input, output) {
		if printBody {
			logformat = "[push-handle] remote-ip: %s | cost-time: %s%s | uri: %-30s |\nRECV:\n packet-length: %d\n body[-json]: %s\n"
			printFunc(logformat, s.RemoteIp(), costTime, slowStr, input.Header.Uri, input.Length, bodyLogBytes(input))
		} else {
			logformat = "[push-handle] remote-ip: %s | cost-time: %s%s | uri: %-30s |\nRECV:\n packet-length: %d\n"
			printFunc(logformat, s.RemoteIp(), costTime, slowStr, input.Header.Uri, input.Length)
		}

	} else if isPullLaunch(input, output) {
		if printBody {
			logformat = "[pull-launch] remote-ip: %s | cost-time: %s%s | uri: %-30s |\nSEND:\n packet-length: %d\n body[-json]: %s\nRECV:\n status: %s %s\n packet-length: %d\n body[-json]: %s\n"
			printFunc(logformat, s.RemoteIp(), costTime, slowStr, output.Header.Uri, output.Length, bodyLogBytes(output), colorCode(input.Header.StatusCode), input.Header.Status, input.Length, bodyLogBytes(input))
		} else {
			logformat = "[pull-launch] remote-ip: %s | cost-time: %s%s | uri: %-30s |\nSEND:\n packet-length: %d\nRECV:\n status: %s %s\n packet-length: %d\n"
			printFunc(logformat, s.RemoteIp(), costTime, slowStr, output.Header.Uri, output.Length, colorCode(input.Header.StatusCode), input.Header.Status, input.Length)
		}

	} else if isPullHandle(input, output) {
		if printBody {
			logformat = "[pull-handle] remote-ip: %s | cost-time: %s%s | uri: %-30s |\nRECV:\n packet-length: %d\n body[-json]: %s\nSEND:\n status: %s %s\n packet-length: %d\n body[-json]: %s\n"
			printFunc(logformat, s.RemoteIp(), costTime, slowStr, input.Header.Uri, input.Length, bodyLogBytes(input), colorCode(output.Header.StatusCode), output.Header.Status, output.Length, bodyLogBytes(output))
		} else {
			logformat = "[pull-handle] remote-ip: %s | cost-time: %s%s | uri: %-30s |\nRECV:\n packet-length: %d\nSEND:\n status: %s %s\n packet-length: %d\n"
			printFunc(logformat, s.RemoteIp(), costTime, slowStr, input.Header.Uri, input.Length, colorCode(output.Header.StatusCode), output.Header.Status, output.Length)
		}
	}
}

func bodyLogBytes(packet *socket.Packet) []byte {
	switch v := packet.Body.(type) {
	case []byte:
		if len(v) == 0 || !isJsonBody(packet) {
			return v
		}
		buf := bytes.NewBuffer(make([]byte, 0, len(v)-1))
		err := json.Indent(buf, v[1:], "", "  ")
		if err != nil {
			return v
		}
		return buf.Bytes()
	case *[]byte:
		if len(*v) == 0 || !isJsonBody(packet) {
			return *v
		}
		buf := bytes.NewBuffer(make([]byte, 0, len(*v)-1))
		err := json.Indent(buf, (*v)[1:], "", "  ")
		if err != nil {
			return *v
		}
		return buf.Bytes()
	default:
		b, _ := jsoniter.MarshalIndent(v, "", "  ")
		return b
	}
}

func isJsonBody(packet *socket.Packet) bool {
	if packet != nil && packet.BodyCodec == "json" {
		return true
	}
	return false
}

func colorCode(code int32) string {
	switch {
	case code >= 500 || code < 200:
		return color.Red(code)
	case code >= 400:
		return color.Magenta(code)
	case code >= 300:
		return color.Grey(code)
	default:
		return color.Green(code)
	}
}

// SessionHub sessions hub
type SessionHub struct {
	// key: session id (ip, name and so on)
	// value: *session
	sessions goutil.Map
}

// newSessionHub creates a new sessions hub.
func newSessionHub() *SessionHub {
	chub := &SessionHub{
		sessions: goutil.AtomicMap(),
	}
	return chub
}

// Set sets a *session.
func (sh *SessionHub) Set(sess *session) {
	_sess, loaded := sh.sessions.LoadOrStore(sess.Id(), sess)
	if !loaded {
		return
	}
	sh.sessions.Store(sess.Id(), sess)
	if oldSess := _sess.(*session); sess != oldSess {
		oldSess.Close()
	}
}

// Get gets *session by id.
// If second returned arg is false, mean the *session is not found.
func (sh *SessionHub) Get(id string) (*session, bool) {
	_sess, ok := sh.sessions.Load(id)
	if !ok {
		return nil, false
	}
	return _sess.(*session), true
}

// Range calls f sequentially for each id and *session present in the session hub.
// If f returns false, range stops the iteration.
func (sh *SessionHub) Range(f func(*session) bool) {
	sh.sessions.Range(func(key, value interface{}) bool {
		return f(value.(*session))
	})
}

// Random gets a *session randomly.
// If third returned arg is false, mean no *session is exist.
func (sh *SessionHub) Random() (*session, bool) {
	_, sess, exist := sh.sessions.Random()
	if !exist {
		return nil, false
	}
	return sess.(*session), true
}

// Len returns the length of the session hub.
// Note: the count implemented using sync.Map may be inaccurate.
func (sh *SessionHub) Len() int {
	return sh.sessions.Len()
}

// Delete deletes the *session for a id.
func (sh *SessionHub) Delete(id string) {
	sh.sessions.Delete(id)
}

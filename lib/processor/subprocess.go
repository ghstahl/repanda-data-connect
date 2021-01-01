package processor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jeffail/benthos/v3/internal/docs"
	"github.com/Jeffail/benthos/v3/lib/log"
	"github.com/Jeffail/benthos/v3/lib/message/tracing"
	"github.com/Jeffail/benthos/v3/lib/metrics"
	"github.com/Jeffail/benthos/v3/lib/response"
	"github.com/Jeffail/benthos/v3/lib/types"
	olog "github.com/opentracing/opentracing-go/log"
)

//------------------------------------------------------------------------------

func init() {
	Constructors[TypeSubprocess] = TypeSpec{
		constructor: NewSubprocess,
		Categories: []Category{
			CategoryIntegration,
		},
		Summary: `
Executes a command as a subprocess and, for each message, will pipe its contents to the stdin stream of the process followed by a newline.`,
		Description: `
The subprocess must then either return a line over stdout or stderr. If a response is returned over stdout then its contents will replace the message. If a response is instead returned from stderr it will be logged and the message will continue unchanged and will be [marked as failed](/docs/configuration/error_handling).

The execution environment of the subprocess is the same as the Benthos instance, including environment variables and the current working directory.

The field ` + "`max_buffer`" + ` defines the maximum response size able to be read from the subprocess. This value should be set significantly above the real expected maximum response size.

## Subprocess requirements

It is required that subprocesses flush their stdout and stderr pipes for each line. Benthos will attempt to keep the process alive for as long as the pipeline is running. If the process exits early it will be restarted.

## Messages containing line breaks

If a message contains line breaks each line of the message is piped to the subprocess and flushed, and a response is expected from the subprocess before another line is fed in.`,
		FieldSpecs: docs.FieldSpecs{
			docs.FieldCommon("name", "The command to execute as a subprocess.", "cat", "sed", "awk"),
			docs.FieldCommon("args", "A list of arguments to provide the command."),
			docs.FieldAdvanced("max_buffer", "The maximum expected response size."),
			docs.FieldAdvanced("codec_send", "The data transfer codec (stdin of the subprocess)"),
			docs.FieldAdvanced("codec_recv", "The data transfer codec (stdout of the subprocess)"),
			partsFieldSpec,
		},
	}
}

//------------------------------------------------------------------------------

// SubprocessConfig contains configuration fields for the Subprocess processor.
type SubprocessConfig struct {
	Parts     []int    `json:"parts" yaml:"parts"`
	Name      string   `json:"name" yaml:"name"`
	Args      []string `json:"args" yaml:"args"`
	MaxBuffer int      `json:"max_buffer" yaml:"max_buffer"`
	CodecSend string   `json:"codec_send" yaml:"codec_send"`
	CodecRecv string   `json:"codec_recv" yaml:"codec_recv"`
}

// NewSubprocessConfig returns a SubprocessConfig with default values.
func NewSubprocessConfig() SubprocessConfig {
	return SubprocessConfig{
		Parts:     []int{},
		Name:      "cat",
		Args:      []string{},
		MaxBuffer: bufio.MaxScanTokenSize,
		CodecSend: "lines",
		CodecRecv: "lines",
	}
}

//------------------------------------------------------------------------------

// Subprocess is a processor that executes a command.
type Subprocess struct {
	subprocClosed int32

	log   log.Modular
	stats metrics.Type

	conf    SubprocessConfig
	subproc *subprocWrapper

	mut sync.Mutex

	mCount     metrics.StatCounter
	mErr       metrics.StatCounter
	mSent      metrics.StatCounter
	mBatchSent metrics.StatCounter
}

// NewSubprocess returns a Subprocess processor.
func NewSubprocess(
	conf Config, mgr types.Manager, log log.Modular, stats metrics.Type,
) (Type, error) {
	return newSubprocess(conf.Subprocess, mgr, log, stats)
}

func newSubprocess(
	conf SubprocessConfig, mgr types.Manager, log log.Modular, stats metrics.Type,
) (Type, error) {
	e := &Subprocess{
		log:        log,
		stats:      stats,
		conf:       conf,
		mCount:     stats.GetCounter("count"),
		mErr:       stats.GetCounter("error"),
		mSent:      stats.GetCounter("sent"),
		mBatchSent: stats.GetCounter("batch.sent"),
	}
	var err error
	if e.subproc, err = newSubprocWrapper(conf.Name, conf.Args, e.conf.MaxBuffer, conf.CodecRecv, log); err != nil {
		return nil, err
	}
	return e, nil
}

//------------------------------------------------------------------------------

type subprocWrapper struct {
	name      string
	args      []string
	maxBuf    int
	codecRecv string

	logger log.Modular

	cmdMut      sync.Mutex
	cmdExitChan chan struct{}
	stdoutChan  chan []byte
	stderrChan  chan []byte

	cmd         *exec.Cmd
	cmdStdin    io.WriteCloser
	cmdCancelFn func()

	closeChan  chan struct{}
	closedChan chan struct{}
}

func newSubprocWrapper(name string, args []string, maxBuf int, codecRecv string, log log.Modular) (*subprocWrapper, error) {
	s := &subprocWrapper{
		name:       name,
		args:       args,
		maxBuf:     maxBuf,
		codecRecv:  codecRecv,
		logger:     log,
		closeChan:  make(chan struct{}),
		closedChan: make(chan struct{}),
	}
	if err := s.start(); err != nil {
		return nil, err
	}
	go func() {
		defer func() {
			s.stop()
			close(s.closedChan)
		}()
		for {
			select {
			case <-s.cmdExitChan:
				log.Warnln("Subprocess exited")
				s.stop()

				// Flush channels
				var msgBytes []byte
				for stdoutMsg := range s.stdoutChan {
					msgBytes = append(msgBytes, stdoutMsg...)
				}
				if len(msgBytes) > 0 {
					log.Infoln(string(msgBytes))
				}
				msgBytes = nil
				for stderrMsg := range s.stderrChan {
					msgBytes = append(msgBytes, stderrMsg...)
				}
				if len(msgBytes) > 0 {
					log.Errorln(string(msgBytes))
				}

				s.start()
			case <-s.closeChan:
				return
			}
		}
	}()
	return s, nil
}

var maxInt = (1<<bits.UintSize)/2 - 1

func lengthPrefixedUInt32BESplitFunc(data []byte, atEOF bool) (advance int, token []byte, err error) {
	const prefixBytes int = 4
	if atEOF {
		return 0, nil, nil
	}
	if len(data) < prefixBytes {
		// request more data
		return 0, nil, nil
	}
	l := binary.BigEndian.Uint32(data)
	if l > (uint32(maxInt) - uint32(prefixBytes)) {
		return 0, nil, errors.New("number of bytes to read exceeds representable range of go int datatype")
	}
	bytesToRead := int(l)

	if len(data)-prefixBytes >= bytesToRead {
		return prefixBytes + bytesToRead, data[prefixBytes : prefixBytes+bytesToRead], nil
	} else {
		// request more data
		return 0, nil, nil
	}
}
func netstringSplitFunc(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF {
		return 0, nil, nil
	}

	if i := bytes.IndexByte(data, ':'); i >= 0 {
		if i == 0 {
			return 0, nil, errors.New("encountered invalid netstring: netstring starts with colon (':')")
		}
		l, err := strconv.ParseUint(string(data[0:i]), 10, bits.UintSize-1)
		if err != nil {
			return 0, nil, errors.New(fmt.Sprintf("encountered invalid netstring: unable to decode length '%v'", string(data[0:i])))
		}
		bytesToRead := int(l)

		if len(data) > i+1+bytesToRead {
			if data[i+1+bytesToRead] != ',' {
				return 0, nil, errors.New("encountered invalid netstring: trailing comma-character is missing")
			}
			return i + 1 + bytesToRead + 1, data[i+1 : i+1+bytesToRead], nil
		}
	}
	// request more data
	return 0, nil, nil
}

func (s *subprocWrapper) start() error {
	s.cmdMut.Lock()
	defer s.cmdMut.Unlock()

	var err error
	cmdCtx, cmdCancelFn := context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			cmdCancelFn()
		}
	}()

	cmd := exec.CommandContext(cmdCtx, s.name, s.args...)
	var cmdStdin io.WriteCloser
	if cmdStdin, err = cmd.StdinPipe(); err != nil {
		return err
	}
	var cmdStdout, cmdStderr io.ReadCloser
	if cmdStdout, err = cmd.StdoutPipe(); err != nil {
		return err
	}
	if cmdStderr, err = cmd.StderrPipe(); err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}

	s.cmd = cmd
	s.cmdStdin = cmdStdin
	s.cmdCancelFn = cmdCancelFn

	cmdExitChan := make(chan struct{})
	stdoutChan := make(chan []byte)
	stderrChan := make(chan []byte)

	go func() {
		defer func() {
			s.cmdMut.Lock()
			if cmdExitChan != nil {
				close(cmdExitChan)
				cmdExitChan = nil
			}
			close(stdoutChan)
			s.cmdMut.Unlock()
		}()

		scanner := bufio.NewScanner(cmdStdout)
		switch s.codecRecv {
		case "lines":
			// bufio Scanner uses ScanLines as default function
			break
		case "length_prefixed_uint32_be":
			scanner.Split(lengthPrefixedUInt32BESplitFunc)
			break
		case "netstring":
			scanner.Split(netstringSplitFunc)
			break
		default:
			s.logger.Errorf("Invalid codec_recv option: '%v' is not one of ('lines','length_prefixed_uint32_be','netstring')\n", s.codecRecv)
		}
		if s.maxBuf != bufio.MaxScanTokenSize {
			scanner.Buffer(nil, s.maxBuf)
		}
		for scanner.Scan() {
			stdoutChan <- scanner.Bytes()
		}
		if err := scanner.Err(); err != nil {
			s.logger.Errorf("Failed to read subprocess output: %v\n", err)
		}
	}()
	go func() {
		defer func() {
			s.cmdMut.Lock()
			if cmdExitChan != nil {
				close(cmdExitChan)
				cmdExitChan = nil
			}
			close(stderrChan)
			s.cmdMut.Unlock()
		}()

		scanner := bufio.NewScanner(cmdStderr)
		if s.maxBuf != bufio.MaxScanTokenSize {
			scanner.Buffer(nil, s.maxBuf)
		}
		for scanner.Scan() {
			stderrChan <- scanner.Bytes()
		}
		if err := scanner.Err(); err != nil {
			s.logger.Errorf("Failed to read subprocess error output: %v\n", err)
		}
	}()

	s.cmdExitChan = cmdExitChan
	s.stdoutChan = stdoutChan
	s.stderrChan = stderrChan
	s.logger.Infoln("Subprocess started")
	return nil
}

func (s *subprocWrapper) stop() error {
	s.cmdMut.Lock()
	var err error
	if s.cmd != nil {
		s.cmdCancelFn()
		err = s.cmd.Wait()
		s.cmd = nil
		s.cmdStdin = nil
		s.cmdCancelFn = func() {}
	}
	s.cmdMut.Unlock()
	return err
}

func (s *subprocWrapper) Send(prolog []byte, payload []byte, epilog []byte) ([]byte, error) {
	s.cmdMut.Lock()
	stdin := s.cmdStdin
	outChan := s.stdoutChan
	errChan := s.stderrChan
	s.cmdMut.Unlock()

	if stdin == nil {
		return nil, types.ErrTypeClosed
	}
	if prolog != nil {
		if _, err := stdin.Write(prolog); err != nil {
			return nil, err
		}
	}
	if _, err := stdin.Write(payload); err != nil {
		return nil, err
	}
	if epilog != nil {
		if _, err := stdin.Write(epilog); err != nil {
			return nil, err
		}
	}

	var outBytes, errBytes []byte
	var open bool
	select {
	case outBytes, open = <-outChan:
	case errBytes, open = <-errChan:
		tout := time.After(time.Second)
		var errBuf bytes.Buffer
		errBuf.Write(errBytes)
	flushErrLoop:
		for open {
			select {
			case errBytes, open = <-errChan:
				errBuf.Write(errBytes)
			case <-tout:
				break flushErrLoop
			}
		}
		errBytes = errBuf.Bytes()
	}

	if !open {
		return nil, types.ErrTypeClosed
	}
	if len(errBytes) > 0 {
		return nil, errors.New(string(errBytes))
	}
	return outBytes, nil
}

//------------------------------------------------------------------------------
var newLineBytes = []byte("\n")
var commaBytes = []byte(",")

// ProcessMessage logs an event and returns the message unchanged.
func (e *Subprocess) ProcessMessage(msg types.Message) ([]types.Message, types.Response) {
	e.mCount.Incr(1)
	e.mut.Lock()
	defer e.mut.Unlock()

	result := msg.Copy()

	var proc func(int) error
	procLines := func(i int) error {
		span := tracing.CreateChildSpan(TypeSubprocess, result.Get(i))
		defer span.Finish()

		results := [][]byte{}
		splitMsg := bytes.Split(result.Get(i).Get(), newLineBytes)
		for j, p := range splitMsg {
			if len(p) == 0 && len(splitMsg) > 1 && j == (len(splitMsg)-1) {
				results = append(results, []byte(""))
				continue
			}
			res, err := e.subproc.Send(nil, p, newLineBytes)
			if err != nil {
				e.log.Errorf("Failed to send message to subprocess: %v\n", err)
				e.mErr.Incr(1)
				span.LogFields(
					olog.String("event", "error"),
					olog.String("type", err.Error()),
				)
				FlagErr(result.Get(i), err)
				results = append(results, p)
			} else {
				results = append(results, res)
			}
		}
		result.Get(i).Set(bytes.Join(results, newLineBytes))
		return nil
	}
	switch e.conf.CodecSend {
	case "lines":
		proc = procLines
		break
	case "length_prefixed_uint32_be":
		proc = func(i int) error {
			span := tracing.CreateChildSpan(TypeSubprocess, result.Get(i))
			defer span.Finish()
			const prefixBytes int = 4

			lenBuf := make([]byte, prefixBytes)
			m := result.Get(i).Get()
			binary.BigEndian.PutUint32(lenBuf, uint32(len(m)))

			res, err := e.subproc.Send(lenBuf, m, nil)
			if err != nil {
				e.log.Errorf("Failed to send message to subprocess: %v\n", err)
				_ = e.mErr.Incr(1)
				span.LogFields(
					olog.String("event", "error"),
					olog.String("type", err.Error()),
				)
				FlagErr(result.Get(i), err)
				result.Get(i).Set(m)
			} else {
				result.Get(i).Set(res)
			}
			return nil
		}
		break
	case "netstring":
		proc = func(i int) error {
			span := tracing.CreateChildSpan(TypeSubprocess, result.Get(i))
			defer span.Finish()

			lenBuf := make([]byte, 0)
			m := result.Get(i).Get()
			lenBuf = append(strconv.AppendUint(lenBuf, uint64(len(m)), 10), ':')
			res, err := e.subproc.Send(lenBuf, m, commaBytes)
			if err != nil {
				e.log.Errorf("Failed to send message to subprocess: %v\n", err)
				e.mErr.Incr(1)
				span.LogFields(
					olog.String("event", "error"),
					olog.String("type", err.Error()),
				)
				FlagErr(result.Get(i), err)
				result.Get(i).Set(m)
			} else {
				result.Get(i).Set(res)
			}
			return nil
		}
		break
	default:
		e.log.Errorf("Invalid codec_send option: '%v' is not one of ('lines','length_prefixed_uint32_be','netstring^)\n", e.conf.CodecSend)
		proc = procLines
	}

	if len(e.conf.Parts) == 0 {
		for i := 0; i < msg.Len(); i++ {
			if err := proc(i); err != nil {
				e.mErr.Incr(1)
				return nil, response.NewError(err)
			}
		}
	} else {
		for _, i := range e.conf.Parts {
			if err := proc(i); err != nil {
				e.mErr.Incr(1)
				return nil, response.NewError(err)
			}
		}
	}

	e.mSent.Incr(int64(result.Len()))
	e.mBatchSent.Incr(1)

	msgs := [1]types.Message{result}
	return msgs[:], nil
}

// CloseAsync shuts down the processor and stops processing requests.
func (e *Subprocess) CloseAsync() {
	if atomic.CompareAndSwapInt32(&e.subprocClosed, 0, 1) {
		close(e.subproc.closeChan)
	}
}

// WaitForClose blocks until the processor has closed down.
func (e *Subprocess) WaitForClose(timeout time.Duration) error {
	select {
	case <-time.After(timeout):
		return fmt.Errorf("subprocess failed to close in allotted time: %w", types.ErrTimeout)
	case <-e.subproc.closedChan:
	}
	return nil
}

//------------------------------------------------------------------------------

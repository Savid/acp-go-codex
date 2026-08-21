package codexacp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"

	"github.com/coder/acp-go-sdk"
)

const establishmentHookParam = "_acp_go_codex_establishment_hook"
const establishmentHookLimit = 256
const establishmentFrameLimit = 10 * 1024 * 1024

var errEstablishmentResponseFailed = errors.New("session establishment response was not delivered")
var errEstablishmentResponseMalformed = errors.New("session establishment response framing was malformed")
var errEstablishmentCancelled = errors.New("session lifecycle establishment was cancelled")
var errEstablishmentFrameTooLarge = errors.New("ACP establishment frame exceeds the fixed limit")

type establishmentHooks struct {
	log *slog.Logger
	mu  sync.Mutex
	all map[string]*establishmentObligation
}

type establishmentObligation struct {
	hooks      *establishmentHooks
	responseID string

	mu      sync.Mutex
	session *session
	done    chan struct{}
	once    sync.Once
}

type establishmentContextKey struct{}

func withEstablishmentObligation(ctx context.Context, obligation *establishmentObligation) context.Context {
	if obligation == nil {
		return ctx
	}

	return context.WithValue(ctx, establishmentContextKey{}, obligation)
}

func establishmentFromContext(ctx context.Context) *establishmentObligation {
	obligation, _ := ctx.Value(establishmentContextKey{}).(*establishmentObligation)

	return obligation
}

func newEstablishmentHooks(log *slog.Logger) *establishmentHooks {
	return &establishmentHooks{log: log, all: make(map[string]*establishmentObligation)}
}

func (h *establishmentHooks) wrap(writer io.Writer) io.Writer {
	return &establishmentWriter{writer: writer, hooks: h}
}

func (h *establishmentHooks) reserve(responseID string) (*establishmentObligation, error) {
	if responseID == "" {
		return nil, errors.New("session establishment response id is required")
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if _, exists := h.all[responseID]; exists {
		return nil, errors.New("session establishment response id is already outstanding")
	}

	if len(h.all) == establishmentHookLimit {
		return nil, errors.New("session establishment response registry is full")
	}

	obligation := &establishmentObligation{
		hooks: h, responseID: responseID, done: make(chan struct{}),
	}
	h.all[responseID] = obligation

	return obligation, nil
}

func (h *establishmentHooks) complete(data []byte, writeErr error) {
	frame, err := decodeExactJSONObject(data)
	if err != nil {
		return
	}

	rawID, hasID := frame["id"]
	if !hasID || !validJSONRPCID(rawID) {
		return
	}

	if _, hasMethod := frame["method"]; hasMethod {
		return
	}

	responseID := establishmentResponseID(&rawID)
	if !rawJSONStringEquals(frame["jsonrpc"], "2.0") {
		h.failMatching(responseID, errEstablishmentResponseMalformed)

		return
	}

	_, hasResult := frame["result"]
	_, hasError := frame["error"]

	if len(frame) != 3 || hasResult == hasError || hasError && !validJSONRPCError(frame["error"]) {
		h.failMatching(responseID, errEstablishmentResponseMalformed)

		return
	}

	h.mu.Lock()

	obligation := h.all[responseID]
	if obligation != nil {
		delete(h.all, responseID)
	}
	h.mu.Unlock()

	if obligation == nil {
		return
	}

	if writeErr == nil && hasError {
		writeErr = errEstablishmentResponseFailed
	}

	obligation.finish(writeErr)
}

func (h *establishmentHooks) failMatching(responseID string, cause error) {
	h.mu.Lock()

	obligation := h.all[responseID]
	if obligation != nil {
		delete(h.all, responseID)
	}
	h.mu.Unlock()

	if obligation != nil {
		obligation.finish(cause)
	}
}

func (h *establishmentHooks) cancel(obligation *establishmentObligation, cause error) {
	if obligation == nil {
		return
	}

	h.mu.Lock()
	if h.all[obligation.responseID] == obligation {
		delete(h.all, obligation.responseID)
	}
	h.mu.Unlock()
	obligation.finish(cause)
}

func (h *establishmentHooks) failAll(cause error) {
	h.mu.Lock()

	pending := make([]*establishmentObligation, 0, len(h.all))
	for id, obligation := range h.all {
		delete(h.all, id)

		pending = append(pending, obligation)
	}
	h.mu.Unlock()

	for _, obligation := range pending {
		obligation.finish(cause)
	}
}

func (o *establishmentObligation) bind(session *session) error {
	if o == nil || session == nil {
		return errors.New("session lifecycle establishment obligation is unavailable")
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	select {
	case <-o.done:
		return errEstablishmentCancelled
	default:
	}

	if o.session != nil && o.session != session {
		return errors.New("session lifecycle establishment obligation changed owner")
	}

	o.session = session

	return nil
}

func (o *establishmentObligation) finish(err error) {
	if o == nil {
		return
	}

	o.once.Do(func() {
		go func() {
			defer close(o.done)
			defer recoverAgentGoroutine(context.Background(), o.hooks.log, "Codex session establishment")

			o.mu.Lock()
			session := o.session
			o.mu.Unlock()

			if session != nil {
				_ = session.completeLifecycleEstablishment(context.Background(), o, err)
			}
		}()
	})
}

func (o *establishmentObligation) wait(ctx context.Context) error {
	if o == nil {
		return nil
	}

	select {
	case <-o.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type establishmentWriter struct {
	writer io.Writer
	hooks  *establishmentHooks
}

func (w *establishmentWriter) Write(data []byte) (int, error) {
	written, err := w.writer.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}

	w.hooks.complete(data, err)

	return written, err
}

type establishmentTagReader struct {
	reader  *bufio.Reader
	pending []byte
	err     error
}

func newEstablishmentTagReader(reader io.Reader) *establishmentTagReader {
	return &establishmentTagReader{reader: bufio.NewReader(reader)}
}

func (r *establishmentTagReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		if r.err != nil {
			err := r.err
			r.err = nil

			return 0, err
		}

		line, err := r.readFrame()
		if len(line) == 0 {
			return 0, err
		}

		tagged, tagErr := tagEstablishingRequest(line)
		if tagErr != nil {
			return 0, tagErr
		}

		r.pending = tagged

		if err != nil {
			r.err = err
		}
	}

	read := copy(p, r.pending)
	r.pending = r.pending[read:]

	return read, nil
}

func (r *establishmentTagReader) readFrame() ([]byte, error) {
	frame := make([]byte, 0, 64*1024)
	overflow := false

	for {
		fragment, err := r.reader.ReadSlice('\n')
		if !overflow {
			if len(frame)+len(fragment) > establishmentFrameLimit {
				overflow = true
				frame = nil
			} else {
				frame = append(frame, fragment...)
			}
		}

		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case overflow:
			return nil, errEstablishmentFrameTooLarge
		default:
			return frame, err
		}
	}
}

func tagEstablishingRequest(line []byte) ([]byte, error) {
	frame, _ := decodeExactJSONObject(line)
	if len(frame) != 4 || !rawJSONStringEquals(frame["jsonrpc"], "2.0") ||
		!validJSONRPCID(frame["id"]) || !validJSONObject(frame["params"]) {
		return line, nil
	}

	var method string

	validMethod := json.Unmarshal(frame["method"], &method) == nil
	if !validMethod || !establishingMethod(method) {
		return line, nil
	}

	methodJSON, _ := json.Marshal(method)
	hookID, _ := json.Marshal(establishmentResponseID(rawMessagePointer(frame["id"])))
	params := bytes.TrimSpace(frame["params"])
	inner := bytes.TrimSpace(params[1 : len(params)-1])
	newline := bytes.HasSuffix(line, []byte("\n"))

	const (
		prefix       = `{"jsonrpc":"2.0","id":`
		methodPrefix = `,"method":`
		paramsPrefix = `,"params":{` + `"` + establishmentHookParam + `":`
	)

	encodedLen := len(prefix) + len(bytes.TrimSpace(frame["id"])) + len(methodPrefix) + len(methodJSON) +
		len(paramsPrefix) + len(hookID) + 2
	if len(inner) != 0 {
		encodedLen += 1 + len(inner)
	}

	if newline {
		encodedLen++
	}

	if encodedLen > establishmentFrameLimit {
		return nil, errEstablishmentFrameTooLarge
	}

	encoded := make([]byte, 0, encodedLen)
	encoded = append(encoded, prefix...)
	encoded = append(encoded, bytes.TrimSpace(frame["id"])...)
	encoded = append(encoded, methodPrefix...)
	encoded = append(encoded, methodJSON...)
	encoded = append(encoded, paramsPrefix...)
	encoded = append(encoded, hookID...)

	if len(inner) != 0 {
		encoded = append(encoded, ',')
		encoded = append(encoded, inner...)
	}

	encoded = append(encoded, '}', '}')
	if newline {
		encoded = append(encoded, '\n')
	}

	return encoded, nil
}

func decodeExactJSONObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(data)))

	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errEstablishmentResponseMalformed
	}

	members := make(map[string]json.RawMessage)

	for decoder.More() {
		nameToken, tokenErr := decoder.Token()
		if tokenErr != nil {
			return nil, errEstablishmentResponseMalformed
		}

		name, _ := nameToken.(string)

		if _, duplicate := members[name]; duplicate {
			return nil, errEstablishmentResponseMalformed
		}

		var value json.RawMessage

		if decodeErr := decoder.Decode(&value); decodeErr != nil {
			return nil, errEstablishmentResponseMalformed
		}

		members[name] = value
	}

	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errEstablishmentResponseMalformed
	}

	if decoder.Decode(new(any)) != io.EOF {
		return nil, errEstablishmentResponseMalformed
	}

	return members, nil
}

func rawJSONStringEquals(raw json.RawMessage, expected string) bool {
	var value string

	return json.Unmarshal(raw, &value) == nil && value == expected
}

func validJSONRPCID(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	decoder.UseNumber()

	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return false
	}

	switch value.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}

func validJSONObject(raw json.RawMessage) bool {
	_, err := decodeExactJSONObject(raw)

	return err == nil
}

func validJSONRPCError(raw json.RawMessage) bool {
	members, err := decodeExactJSONObject(raw)
	if err != nil || len(members) < 2 || len(members) > 3 {
		return false
	}

	if len(members) == 3 {
		if _, ok := members[jsonFieldData]; !ok {
			return false
		}
	}

	for name := range members {
		if name != jsonFieldCode && name != jsonFieldMessage && name != jsonFieldData {
			return false
		}
	}

	var code json.Number

	decoder := json.NewDecoder(bytes.NewReader(members[jsonFieldCode]))
	decoder.UseNumber()

	if decoder.Decode(&code) != nil {
		return false
	}

	if _, err := strconv.ParseInt(string(code), 10, 64); err != nil {
		return false
	}

	var message string

	return json.Unmarshal(members[jsonFieldMessage], &message) == nil
}

func rawMessagePointer(raw json.RawMessage) *json.RawMessage { return &raw }

func establishingMethod(method string) bool {
	switch method {
	case acp.AgentMethodSessionNew, acp.AgentMethodSessionLoad, acp.AgentMethodSessionResume, ForkSessionMethod:
		return true
	default:
		return false
	}
}

func establishmentResponseID(id *json.RawMessage) string {
	return string(bytes.TrimSpace(*id))
}

func establishmentHookID(params json.RawMessage) string {
	var members map[string]json.RawMessage
	if json.Unmarshal(params, &members) != nil {
		return ""
	}

	var responseID string
	if json.Unmarshal(members[establishmentHookParam], &responseID) != nil {
		return ""
	}

	return responseID
}

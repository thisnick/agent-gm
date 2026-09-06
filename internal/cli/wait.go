package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// The four things `--wait-for` can mean (spec section 11.3).
const (
	WaitSent      = "sent"
	WaitDelivered = "delivered"
	WaitRead      = "read"
	WaitTerminal  = "terminal"
)

// DefaultWaitTimeout bounds a `--wait` that was given no --timeout.
const DefaultWaitTimeout = 60 * time.Second

// terminalOperationStatuses are the operation statuses that settle it
// (spec section 6.4). `unknown` is settled and is NOT success: it is a crash
// between the commit and the backend call, corrected later if an echo lands.
var terminalOperationStatuses = map[string]bool{
	"succeeded": true, "failed": true, "unknown": true,
}

// terminalDeliveryStates are the message delivery states that satisfy
// `--wait-for terminal` (spec section 11.3).
var terminalDeliveryStates = map[string]bool{
	"delivered": true, "read": true, "failed": true, "canceled": true, "deleted": true,
}

// operationView is the part of the operation object (spec section 6.5) a wait
// reads.
type operationView struct {
	ID             string  `json:"id"`
	Status         string  `json:"status"`
	Terminal       bool    `json:"terminal"`
	MessageID      *string `json:"message_id"`
	ConversationID *string `json:"conversation_id"`
	Error          *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// waitTarget reads --wait and --wait-for (or --for, on `operations wait`).
//
// `--wait-for` implies `--wait`: asking what to wait for and then not waiting
// would be a silently ignored flag, which is the failure mode section 7.1
// refuses on the wire for the same reason.
func (r *runner) waitTarget(inv *invocation) (string, bool, error) {
	flag := "--wait-for"
	if !inv.has(flag) && inv.has("--for") {
		flag = "--for"
	}
	target := inv.str(flag)
	wait := inv.boolean("--wait") || inv.has(flag)

	if target == "" {
		target = WaitSent
	}
	switch target {
	case WaitSent, WaitDelivered, WaitRead, WaitTerminal:
	default:
		return "", false, &UsageError{Flag: flag,
			Msg: fmt.Sprintf("must be sent, delivered, read or terminal, not %q", target)}
	}
	return target, wait, nil
}

// warnIfSMS is the warning of spec sections 11.3 and 5.5, on stderr.
//
// SMS usually stops at `sent`: the carrier reports nothing further, so an
// agent that asked for `delivered` on an sms_mms conversation can wait until
// its timeout for a state that will never arrive. The warning is on stderr,
// so it does not disturb `--json`.
func (r *runner) warnIfSMS(conversationID, target string) {
	if conversationID == "" || (target != WaitDelivered && target != WaitRead) {
		return
	}
	resp, err := r.client.Do(r.ctx, Request{
		Method: http.MethodGet,
		Path:   "/v1/conversations/" + conversationID,
	})
	if err != nil {
		// The warning is a courtesy; a failure to look the conversation up
		// is not a reason to refuse the send.
		r.out.Verbosef("could not check whether %s is sms_mms: %v", conversationID, err)
		return
	}
	var conv struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(resp.Data, &conv); err != nil {
		return
	}
	if conv.Type != "sms_mms" {
		return
	}
	r.out.Warnf("this is an sms_mms conversation, and SMS usually stops at \"sent\": "+
		"the carrier reports nothing further, so --wait-for %s can wait until the timeout "+
		"for a state that never arrives. --wait-for sent, or --wait-for terminal, is what "+
		"settles here.", target)
}

// waitTimeout is the bound on a wait: --timeout, else the default.
func (r *runner) waitTimeout() time.Duration {
	if r.g.timeoutSet {
		return r.g.timeout
	}
	return DefaultWaitTimeout
}

// maybeWait waits on the operation a mutation returned, when --wait or
// --wait-for was given, and returns the response to emit.
//
// It emits ONCE, whether it waited or not: `--json` puts exactly one JSON
// value on stdout for every command (section 16 Slice 2 test 26), so the
// waited result replaces the immediate one rather than following it.
func (r *runner) maybeWait(inv *invocation, resp *Response) (*Response, error) {
	target, wait, err := r.waitTarget(inv)
	if err != nil {
		return nil, err
	}
	if !wait {
		return resp, nil
	}
	op, ok := operationOf(resp.Data)
	if !ok || op.ID == "" {
		// operation: null -- the conversation was already in the requested
		// state, or the reaction was already there. There is nothing to wait
		// for and nothing was sent (spec section 7.7).
		r.out.Verbosef("the route returned no operation; there is nothing to wait for")
		return resp, nil
	}

	final, err := r.waitLoop(op, target)
	if err != nil {
		return nil, err
	}
	merged, err := mergeOperation(resp.Data, final)
	if err != nil {
		return nil, err
	}
	out := *resp
	out.Data = merged
	return &out, nil
}

// waitLoop polls GET /v1/operations/{id} on the client side, and
// GET /v1/messages/{id} when the target is a MESSAGE state.
//
// `delivered` and `read` are message states, not operation states, so the
// loop keeps going after the operation is terminal. That is deliberate
// (spec section 11.3): `--wait-for terminal` is the flag that means "stop as
// soon as anything is settled".
func (r *runner) waitLoop(op operationView, target string) (json.RawMessage, error) {
	deadline := r.env.Now().Add(r.waitTimeout())
	current := op
	var raw json.RawMessage
	delivery := ""

	for {
		fresh, freshRaw, err := r.readOperation(current.ID)
		if err != nil {
			return nil, err
		}
		current, raw = fresh, freshRaw

		// An operation that genuinely reached failed or unknown is exit 8,
		// and it is the ONLY thing that produces exit 8. phone_not_responding
		// is exit 7 with the operation still pending, and comes from the
		// error envelope, not from here (spec section 11.2).
		if current.Status == "failed" || current.Status == "unknown" {
			detail := ""
			if current.Error != nil {
				detail = current.Error.Message
			}
			return nil, &OperationFailedError{
				OperationID: current.ID, Status: current.Status, Detail: detail}
		}

		if needsMessageState(target) && current.MessageID != nil && *current.MessageID != "" {
			delivery, err = r.readDeliveryState(*current.MessageID)
			if err != nil {
				return nil, err
			}
		}

		if satisfied(target, current, delivery) {
			return raw, nil
		}

		if !r.env.Now().Before(deadline) {
			return nil, &WaitTimeoutError{OperationID: current.ID, WaitFor: target}
		}
		r.out.Verbosef("operation %s is %s; waiting for %s", current.ID, current.Status, target)
		time.Sleep(r.env.PollInterval)
	}
}

func needsMessageState(target string) bool {
	return target == WaitDelivered || target == WaitRead || target == WaitTerminal ||
		target == WaitSent
}

// satisfied is the table of spec section 11.3.
func satisfied(target string, op operationView, delivery string) bool {
	switch target {
	case WaitSent:
		return op.Status == "succeeded" ||
			delivery == "sent" || delivery == "delivered" || delivery == "read"
	case WaitDelivered:
		return delivery == "delivered" || delivery == "read"
	case WaitRead:
		return delivery == "read"
	case WaitTerminal:
		return terminalOperationStatuses[op.Status] || terminalDeliveryStates[delivery]
	default:
		return false
	}
}

func (r *runner) readOperation(id string) (operationView, json.RawMessage, error) {
	resp, err := r.client.Do(r.ctx, Request{
		Method: http.MethodGet,
		Path:   "/v1/operations/" + id,
	})
	if err != nil {
		return operationView{}, nil, err
	}
	var op operationView
	if err := json.Unmarshal(resp.Data, &op); err != nil {
		return operationView{}, nil, &ContractError{
			Msg: "GET /v1/operations/{id} answered something that is not the operation object of spec 6.5"}
	}
	return op, resp.Data, nil
}

func (r *runner) readDeliveryState(messageID string) (string, error) {
	resp, err := r.client.Do(r.ctx, Request{
		Method: http.MethodGet,
		Path:   "/v1/messages/" + messageID,
	})
	if err != nil {
		return "", err
	}
	var msg struct {
		DeliveryState string `json:"delivery_state"`
	}
	if err := json.Unmarshal(resp.Data, &msg); err != nil {
		return "", nil
	}
	return msg.DeliveryState, nil
}

// operationOf pulls the inline operation out of a mutation's data, or reads
// the data as an operation itself.
func operationOf(data json.RawMessage) (operationView, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return operationView{}, false
	}
	if raw, ok := obj["operation"]; ok && string(raw) != "null" {
		var op operationView
		if err := json.Unmarshal(raw, &op); err == nil {
			return op, true
		}
	}
	var op operationView
	if err := json.Unmarshal(data, &op); err == nil && op.ID != "" {
		return op, true
	}
	return operationView{}, false
}

// mergeOperation replaces the operation inside a mutation's data with the
// settled one, keeping everything else the route returned -- the conversation
// `agm conversations start` created, for instance.
func mergeOperation(data, op json.RawMessage) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return op, nil
	}
	if _, ok := obj["operation"]; !ok {
		// The data WAS the operation.
		return op, nil
	}
	obj["operation"] = op
	merged, err := json.Marshal(obj)
	if err != nil {
		return nil, &ContractError{Msg: "the settled operation could not be merged into the result"}
	}
	return merged, nil
}

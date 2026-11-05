package mail

import (
	"errors"
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// Routes are the /v1/mail routes (Node's routes/mail.js).
type Routes struct {
	Service *Service
	Router  *httpapi.Router
}

// Register adds the routes to the /v1 mux.
func (rt *Routes) Register(mux *httpapi.RawMux) {
	route := func(name string, fn httpapi.HandlerFunc) {
		mux.Handle(http.MethodPost, "/mail/"+name, rt.Router.Route("mail", name, fn))
	}
	route("send", rt.send)
	route("check", rt.check)
	route("receive", rt.receive)
	route("edit", rt.edit)
	route("unsend", rt.unsend)
	route("ack", rt.ack)
	route("sent", rt.sent)
	route("history", rt.history)
}

// noBodyRead is V8's TypeError for `req.body.<field>` with no JSON body. The
// routes that destructure the body first use httpapi.NoBody instead.
func noBodyRead(field string) error {
	return errors.New("Cannot read properties of undefined (reading '" + field + "')")
}

// contentField is an optional free-text field, sanitized.
func contentField(f httpapi.Fields, field string) (string, error) {
	s, err := f.OptionalString(field)
	return sanitize.Content(s), err
}

// stringFields reads optional string fields in order, stopping at the first
// that is not a string.
func stringFields(f httpapi.Fields, read func(httpapi.Fields, string) (string, error), names ...string) ([]string, error) {
	out := make([]string, len(names))
	for i, n := range names {
		s, err := read(f, n)
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

func (rt *Routes) send(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("in_reply_to")
	}
	from, done, err := httpapi.Identity(w, r, f, "from_agent", "from_agent does not match authenticated agent")
	if done || err != nil {
		return err
	}
	to, err := httpapi.AgentField(f, "to_agent")
	if err != nil {
		return err
	}
	text, err := stringFields(f, contentField, "subject", "body")
	if err != nil {
		return err
	}
	inReplyTo, err := f.OptionalString("in_reply_to")
	if err != nil {
		return err
	}
	res, err := rt.Service.Send(r.Context(), to, from, text[0], text[1], inReplyTo)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) check(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("agent")
	}
	agent, done, err := httpapi.Identity(w, r, f, "agent", "agent does not match authenticated agent")
	if done || err != nil {
		return err
	}
	res, err := rt.Service.Check(r.Context(), agent)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) receive(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("agent")
	}
	agent, done, err := httpapi.Identity(w, r, f, "agent", "agent does not match authenticated agent")
	if done || err != nil {
		return err
	}
	res, err := rt.Service.Receive(r.Context(), agent, f.Raw("ids"))
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) edit(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	from, done, err := httpapi.Identity(w, r, f, "from_agent", "from_agent does not match authenticated agent")
	if done || err != nil {
		return err
	}
	id, err := f.OptionalString("id")
	if err != nil {
		return err
	}
	text, err := stringFields(f, contentField, "subject", "body")
	if err != nil {
		return err
	}
	res, err := rt.Service.Edit(r.Context(), id, from, text[0], text[1])
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) unsend(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("id")
	}
	from, done, err := httpapi.Identity(w, r, f, "from_agent", "from_agent does not match authenticated agent")
	if done || err != nil {
		return err
	}
	id, err := f.OptionalString("id")
	if err != nil {
		return err
	}
	res, err := rt.Service.Unsend(r.Context(), id, from)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) ack(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("agent")
	}
	agent, done, err := httpapi.Identity(w, r, f, "agent", "agent does not match authenticated agent")
	if done || err != nil {
		return err
	}
	// `ids` is canonical; `message_ids` is the legacy name.
	ids := f.Raw("ids")
	if !f.Truthy("ids") {
		ids = f.Raw("message_ids")
	}
	res, err := rt.Service.Ack(r.Context(), agent, ids)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) sent(w http.ResponseWriter, r *http.Request) error {
	agent, opts, done, err := listArgs(w, r, "to")
	if done || err != nil {
		return err
	}
	res, err := rt.Service.Sent(r.Context(), agent, opts)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) history(w http.ResponseWriter, r *http.Request) error {
	agent, opts, done, err := listArgs(w, r, "from")
	if done || err != nil {
		return err
	}
	res, err := rt.Service.History(r.Context(), agent, opts)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

// listArgs reads the body of sent and history; peerField names the filter.
func listArgs(w http.ResponseWriter, r *http.Request, peerField string) (agent string, opts ListOptions, done bool, err error) {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return "", opts, true, noBodyRead("agent")
	}
	agent, done, err = httpapi.Identity(w, r, f, "agent", "agent does not match authenticated agent")
	if done || err != nil {
		return "", opts, true, err
	}
	peer, err := httpapi.AgentField(f, peerField)
	if err != nil {
		return "", opts, true, err
	}
	return agent, ListOptions{Peer: peer, Limit: f.Raw("limit"), Offset: f.Raw("offset")}, false, nil
}

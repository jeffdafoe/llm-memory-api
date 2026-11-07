package syserrors

import (
	"context"
	"net/http"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// Routes is Node's routes/system.js: POST /v1/system/error/report, which
// any agent or process uses to report an error.
type Routes struct {
	Recorder *Recorder
	Router   *httpapi.Router
	Log      *logger.Logger
}

// reportTimeout bounds the detached report: the handler's message and the
// row are written even if the caller hangs up, as in Node.
const reportTimeout = 30 * time.Second

// Register adds the route to the /v1 mux.
func (rt *Routes) Register(mux *httpapi.RawMux) {
	mux.Handle(http.MethodPost, "/system/error/report", rt.Router.Route("system", "error-report", rt.report))
}

func (rt *Routes) report(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("source")
	}
	// The 400s are written here, not returned, so they leave no error_log
	// row: Node answered them itself instead of throwing.
	if !f.Truthy("source") || !f.Truthy("error_code") {
		httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "Required fields: source, error_code")
		return nil
	}
	source, sourceIsString := f.String("source")
	code, codeIsString := f.String("error_code")
	if !sourceIsString || !codeIsString {
		httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "source and error_code must be strings")
		return nil
	}
	var details *jsutil.Object
	if raw := f.Raw("context"); raw != nil {
		var obj *jsutil.Object
		if len(raw) > 0 && raw[0] == '{' {
			if v, err := jsutil.Parse(raw); err == nil {
				obj, _ = v.(*jsutil.Object)
			}
		}
		if obj == nil {
			httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "context must be a JSON object")
			return nil
		}
		details = obj
	}

	agent := httpapi.StateOf(r).AuthenticatedAgent
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), reportTimeout)
	defer cancel()
	row, err := rt.Recorder.Report(ctx, agent, source, code, details)
	if err != nil {
		return err
	}

	status, _ := row.String("status")
	id, _ := row.Get("id")
	// JSON.stringify drops an undefined agent (a web-user session).
	fields := logger.Fields{}
	if agent != "" {
		fields = append(fields, logger.Field{Key: "agent", Value: agent})
	}
	fields = append(fields,
		logger.Field{Key: "source", Value: source},
		logger.Field{Key: "error_code", Value: code},
		logger.Field{Key: "status", Value: status},
		logger.Field{Key: "id", Value: id},
	)
	rt.Log.Log("system", "error-report", fields)

	return httpapi.WriteJSON(w, http.StatusOK, row)
}

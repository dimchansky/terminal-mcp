package dashboard

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"terminalmcp/internal/viewer"
)

// errStreamReplaced ends a stream that a newer stream of its window replaced.
var errStreamReplaced = errors.New("dashboard: stream replaced")

// serveStream answers a stream request: start errors are JSON answers like
// every other route; after the start, events in SSE format follow until a
// final event, the end of the request or the session, or a stop.
func serveStream(w http.ResponseWriter, r *http.Request, s *viewer.Session, mg *Manager, req *request) {
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	defer context.AfterFunc(s.Context(), func() { cancel(context.Cause(s.Context())) })()
	st, err := mg.OpenStream(ctx, s, req.Window, *req.Generation, req.After)
	if err != nil {
		writeError(w, err)
		return
	}
	vs := viewer.NewStream(w)
	// Released last, before the handler returns: a replacement's late Stop
	// must never reach this connection's next request.
	defer vs.Release()
	// Claim before attaching: a lingering stream of this window still holds
	// the session's stream slot until it is stopped.
	done, err := mg.ClaimStream(ctx, s, req.Window, func() {
		vs.Stop(errStreamReplaced, time.Now())
		cancel(errStreamReplaced)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	defer done()
	release, err := s.Attach(viewer.KindStream, vs)
	if err != nil {
		writeError(w, err)
		return
	}
	defer release()

	rc := http.NewResponseController(w)
	if errors.Is(vs.Extend(time.Now().Add(chunkTimeout)), viewer.ErrStreamStopped) {
		_, _, cause := vs.Stopped()
		if errors.Is(cause, errStreamReplaced) {
			return // Its deadline is now: nothing can be written.
		}
		w.Header().Set("Connection", "close")
		writeError(w, cmp.Or(context.Cause(s.Context()), cause, viewer.ErrStopping))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	write := func(event string, data any) error {
		body, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		return nil
	}
	send := func(event string, data any) error {
		// Every write has its own deadline: a client that stopped reading
		// is cut off instead of holding the handler.
		if err := vs.Extend(time.Now().Add(chunkTimeout)); errors.Is(err, viewer.ErrStreamStopped) {
			return err
		}
		return write(event, data)
	}
	if err := st.Run(ctx, send); err == nil {
		return
	}
	// A server stop with a grace period gets a final event. The session's
	// context ends just before its resources are stopped: write under the
	// stop deadline if the stream is stopped already, else under a short
	// one of our own that the stop can still cut.
	// The session's cause is set before its resources are stopped, even
	// when this request's context has not seen it yet.
	var stop *viewer.StopError
	if errors.As(context.Cause(s.Context()), &stop) {
		extended := vs.Extend(time.Now().Add(time.Second)) == nil
		stopped, deadline, _ := vs.Stopped()
		if extended || (stopped && time.Now().Before(deadline)) {
			_ = write(EventServerStopped, struct {
				Reason viewer.StopReason `json:"reason"`
			}{stop.Reason})
		}
	}
}

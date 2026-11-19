package server

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"

	"switchyard/internal/reqctx"
)

// Audit results.
const (
	// AuditOK is an admin action that took effect.
	AuditOK = "ok"
	// AuditRejected is an admin action refused because of its input, such
	// as an invalid config or an unknown tenant; nothing changed.
	AuditRejected = "rejected"
	// AuditError is an admin action that failed for another reason, such
	// as a store that did not answer; nothing changed.
	AuditError = "error"
)

// auditMsg is the message of every audit line.
const auditMsg = "audit"

// auditLineHead bounds how far into a line IsAuditLine looks for the
// message: slog writes time and level first, then msg.
const auditLineHead = 128

// IsAuditLine reports whether line, an encoded log line in slog's JSON or
// text format, is an audit line (msg "audit"). The app passes it to its
// log writer (telemetry.AsyncOptions.Durable), so that audit lines are not
// dropped when the log queue is full. A false positive only makes a line
// wait for room.
func IsAuditLine(line []byte) bool {
	head := line[:min(len(line), auditLineHead)]
	return bytes.Contains(head, []byte(`"msg":"`+auditMsg+`"`)) || bytes.Contains(head, []byte(" msg="+auditMsg+" "))
}

// Audit writes the audit line of an admin action (DESIGN §17): msg
// "audit", the action (for example "config.reload" or "cache.purge"), its
// result (AuditOK, AuditRejected or AuditError), the client's remote_ip,
// then attrs, through the request's logger, so the line carries its
// request_id. It is logged at INFO, or at WARN unless the result is ok.
// The app's log writer waits for room for it in a full queue, up to a
// bound, rather than dropping it (see IsAuditLine).
// Callers must never pass the request body, a header or a token in attrs.
func Audit(r *http.Request, action, result string, attrs ...slog.Attr) {
	ctx := r.Context()
	logAudit(ctx, reqctx.Logger(ctx), action, result, RemoteIP(r), attrs)
}

// AuditWithout is Audit for an action that has no HTTP request, such as a
// reload on SIGHUP: remote_ip is empty, and the line goes to log.
func AuditWithout(log *slog.Logger, action, result string, attrs ...slog.Attr) {
	logAudit(context.Background(), log, action, result, "", attrs)
}

func logAudit(ctx context.Context, log *slog.Logger, action, result, remoteIP string, attrs []slog.Attr) {
	all := make([]slog.Attr, 0, 3+len(attrs))
	all = append(all, slog.String("action", action), slog.String("result", result), slog.String("remote_ip", remoteIP))
	all = append(all, attrs...)
	level := slog.LevelInfo
	if result != AuditOK {
		level = slog.LevelWarn
	}
	log.LogAttrs(ctx, level, auditMsg, all...)
}

// RemoteIP returns the IP address of the client connection of r, without
// the port: the TCP peer, never a forwarding header, which a client can
// set to anything.
func RemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

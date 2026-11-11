package apierror

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// WriteParam is Write with the envelope's param field set to the name of
// the request parameter at fault, as OpenAI does for validation errors. An
// empty param is encoded as JSON null, exactly like Write. It has the same
// behavior as Write in every other respect.
func WriteParam(w http.ResponseWriter, status int, errType, code, message, param string) {
	if headerWritten(w) {
		return
	}
	if status < 100 || status > 999 {
		status = http.StatusInternalServerError
	}

	env := envelope{Error: body{Message: message, Type: errType}}
	if code != "" {
		env.Error.Code = &code
	}
	if param != "" {
		env.Error.Param = &param
	}
	data, err := json.Marshal(env)
	if err != nil {
		data = []byte(fallbackBody)
	} else {
		data = append(data, '\n')
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

package common

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// FlushResponseWriter keeps each wrapper's flush policy and surfaces network
// flush errors that Gin's legacy http.Flusher otherwise discards.
func FlushResponseWriter(writer gin.ResponseWriter) error {
	if writer == nil {
		return nil
	}
	if flusher, ok := writer.(interface{ FlushError() error }); ok {
		return flusher.FlushError()
	}
	writer.Flush()
	// Legacy wrappers must flush first to preserve their buffered data and
	// status bookkeeping. Then ask the underlying error-aware writer for its
	// stored write result, following the official gateway's compatibility path.
	var underlying http.ResponseWriter = writer
	for {
		if flusher, ok := underlying.(interface{ FlushError() error }); ok {
			return flusher.FlushError()
		}
		unwrapper, ok := underlying.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		underlying = unwrapper.Unwrap()
	}
}

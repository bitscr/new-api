package helper

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
)

const streamPingWriteTimeout = 10 * time.Second

// BeginStreamWriteCancellation interrupts a blocked network write when this
// request/attempt is canceled. Its callback holds a stable response controller,
// never the pooled Gin context, and is joined before the writer can be reused.
// Unsupported custom writers remain owned until their synchronous write ends.
func BeginStreamWriteCancellation(c *gin.Context) func() {
	if c == nil || c.Request == nil || c.Writer == nil {
		return func() {}
	}
	controller := http.NewResponseController(c.Writer)
	requestCtx := c.Request.Context()
	callbackDone := make(chan struct{})
	stopCallback := context.AfterFunc(requestCtx, func() {
		defer close(callbackDone)
		// A speculative expiry cancels upstream reads, not the live client's
		// HTTP/2 stream: an expired write deadline cannot be extended there.
		// Uncommitted staging already prevents writes for this expired attempt.
		if errors.Is(context.Cause(requestCtx), relaycommon.ErrAutoExplorationTimeout) {
			return
		}
		_ = controller.SetWriteDeadline(time.Now())
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			if !stopCallback() {
				<-callbackDone
				// Do not leave a connection deadline on a later keepalive request.
				_ = controller.SetWriteDeadline(time.Time{})
			}
		})
	}
}

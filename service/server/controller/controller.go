package controller

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/v2rayA/v2rayA/common"
	"github.com/v2rayA/v2rayA/kernel/v2ray"
)

var (
	// mutation serialises the requests that rewrite the store or restart
	// the core. Reads do not take it: a touch during a latency test is a
	// database read and needs no turn.
	mutation      = make(chan struct{}, 1)
	mutationWait  = 5 * time.Second
	processingErr = common.Coded("REQUEST_IN_PROGRESS", fmt.Errorf("the last request is being processed"), nil)
)

// beginMutation waits for the turn to change state. When it returns false
// the request has been answered with REQUEST_IN_PROGRESS, or the client
// went away.
//
// The returned release also tells the plugins the node set may have moved.
// Doing it here rather than in each handler means a new mutating endpoint
// cannot forget to announce itself. The event is deduplicated downstream, so
// the several endpoints a single save touches produce one notification.
func beginMutation(ctx *gin.Context) (release func(), ok bool) {
	return BeginMutation(ctx, func() {
		v2ray.NotifyPlugins(v2ray.EventServersChanged)
	})
}

// BeginMutation takes the turn that serialises state-changing requests.
//
// It is exported so the plugin endpoints can queue behind the same lock rather
// than running a core reload concurrently with one. The optional after hook
// runs once the turn is released, which is where a caller announces that the
// node set may have moved.
func BeginMutation(ctx *gin.Context, after func()) (release func(), ok bool) {
	timer := time.NewTimer(mutationWait)
	defer timer.Stop()
	select {
	case mutation <- struct{}{}:
		return func() {
			<-mutation
			if after != nil {
				after()
			}
		}, true
	case <-timer.C:
		common.ResponseError(ctx, processingErr)
		return nil, false
	case <-ctx.Request.Context().Done():
		return nil, false
	}
}

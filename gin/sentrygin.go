package sentrygin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gin-gonic/gin"
)

const (
	// sdkIdentifier is the identifier of the Gin SDK.
	sdkIdentifier = "sentry.go.gin"

	// valuesKey is used as a key to store the Sentry Hub instance on the gin.Context.
	valuesKey = "sentry"

	// transactionKey is used as a key to store the Sentry transaction on the gin.Context.
	transactionKey = "sentry_transaction"
)

type handler struct {
	repanic         bool
	waitForDelivery bool
	timeout         time.Duration
}

type Options struct {
	// Repanic configures whether Sentry should repanic after recovery, in most cases it should be set to true,
	// as gin.Default includes it's own Recovery middleware what handles http responses.
	Repanic bool
	// WaitForDelivery configures whether you want to block the request before moving forward with the response.
	// Because Gin's default Recovery handler doesn't restart the application,
	// it's safe to either skip this option or set it to false.
	WaitForDelivery bool
	// Timeout for the event delivery requests.
	Timeout time.Duration
}

// New returns a function that satisfies gin.HandlerFunc interface
// It can be used with Use() methods.
func New(options Options) gin.HandlerFunc {
	if options.Timeout == 0 {
		options.Timeout = 2 * time.Second
	}

	return (&handler{
		repanic:         options.Repanic,
		timeout:         options.Timeout,
		waitForDelivery: options.WaitForDelivery,
	}).handle
}

func (h *handler) handle(c *gin.Context) {
	ctx := c.Request.Context()
	hub := sentry.GetHubFromContext(ctx)
	if hub == nil {
		hub = sentry.CurrentHub().Clone()
	}

	if client := hub.Client(); client != nil {
		client.SetSDKIdentifier(sdkIdentifier)
	}

	transactionName := c.Request.URL.Path
	transactionSource := sentry.SourceURL

	if fp := c.FullPath(); fp != "" {
		transactionName = fp
		transactionSource = sentry.SourceRoute
	}

	options := []sentry.SpanOption{
		sentry.ContinueTrace(hub, c.GetHeader(sentry.SentryTraceHeader), c.GetHeader(sentry.SentryBaggageHeader)),
		sentry.WithOpName("http.server"),
		sentry.WithTransactionSource(transactionSource),
		sentry.WithSpanOrigin(sentry.SpanOriginGin),
	}

	transaction := sentry.StartTransaction(
		sentry.SetHubOnContext(ctx, hub),
		fmt.Sprintf("%s %s", c.Request.Method, transactionName),
		options...,
	)

	transaction.SetData("http.request.method", c.Request.Method)

	defer func() {
		status := c.Writer.Status()
		transaction.Status = sentry.HTTPtoSpanStatus(status)
		transaction.SetData("http.response.status_code", status)
		transaction.Finish()
	}()

	c.Request = c.Request.WithContext(transaction.Context())
	hub.Scope().SetRequest(c.Request)
	c.Set(valuesKey, hub)
	c.Set(transactionKey, transaction)
	defer h.recoverWithSentry(hub, c.Request)

	c.Next()
}

func (h *handler) recoverWithSentry(hub *sentry.Hub, r *http.Request) {
	if err := recover(); err != nil {
		if !isBrokenPipeError(err) {
			eventID := hub.RecoverWithContext(
				context.WithValue(r.Context(), sentry.RequestContextKey, r),
				err,
			)
			if eventID != nil && h.waitForDelivery {
				hub.Flush(h.timeout)
			}
		}
		if h.repanic {
			panic(err)
		}
	}
}

// Check for a broken connection, as this is what Gin does already.
func isBrokenPipeError(err interface{}) bool {
	if netErr, ok := err.(*net.OpError); ok {
		if sysErr, ok := netErr.Err.(*os.SyscallError); ok {
			if strings.Contains(strings.ToLower(sysErr.Error()), "broken pipe") ||
				strings.Contains(strings.ToLower(sysErr.Error()), "connection reset by peer") {
				return true
			}
		}
	}
	return false
}

// GetHubFromContext retrieves attached *sentry.Hub instance from gin.Context.
func GetHubFromContext(ctx *gin.Context) *sentry.Hub {
	if hub, ok := ctx.Get(valuesKey); ok {
		if hub, ok := hub.(*sentry.Hub); ok {
			return hub
		}
	}
	return nil
}

// SetHubOnContext sets *sentry.Hub instance to gin.Context.
func SetHubOnContext(ctx *gin.Context, hub *sentry.Hub) {
	ctx.Set(valuesKey, hub)
}

// GetSpanFromContext retrieves attached *sentry.Span instance from gin.Context.
// If there is no transaction on echo.Context, it will return nil.
func GetSpanFromContext(ctx *gin.Context) *sentry.Span {
	if span, ok := ctx.Get(transactionKey); ok {
		if span, ok := span.(*sentry.Span); ok {
			return span
		}
	}
	return nil
}

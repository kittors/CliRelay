package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

// ModelGate resolves whether the request may use a model.
type ModelGate func(model string) *interfaces.ErrorMessage

// ModelGateContextKey is the gin context key used by the model restriction middleware.
const ModelGateContextKey = "cliproxy.modelGate"

// ModelGateFromGin returns the gate published for the request, if any.
func ModelGateFromGin(c *gin.Context) ModelGate {
	if c == nil {
		return nil
	}
	value, ok := c.Get(ModelGateContextKey)
	if !ok {
		return nil
	}
	gate, _ := value.(ModelGate)
	return gate
}

package handlers

import "github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"

// FailToolLoopChecksForTest makes every tool-loop check panic until the returned
// function is called, to prove that a defect in the check costs a request its
// protection and nothing more. Tests that use it must not run in parallel.
func FailToolLoopChecksForTest() (restore func()) {
	previous := toolLoopCheck
	toolLoopCheck = func(loopdetect.Protocol, []byte, loopdetect.ToolLimits) (loopdetect.ToolLoop, bool) {
		panic("injected defect in the tool-loop check")
	}
	return func() { toolLoopCheck = previous }
}

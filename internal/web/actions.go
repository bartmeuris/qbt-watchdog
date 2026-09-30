package web

import "qbt-watchdog/internal/config"

// ManualActions is the seam the UI uses to queue an operator's manual action.
// It returns the current decision for display and an error if rejected.
type ManualActions interface {
	Force(hash string, action config.Action, reason string) (decision string, err error)
}

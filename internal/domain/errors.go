package domain

import "errors"

var (
	ErrNotFound          = errors.New("not found")
	ErrRunInProgress     = errors.New("another update is already running")
	ErrInvalidTransition = errors.New("invalid status change")
)

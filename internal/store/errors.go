package store

import "errors"

var ErrNotFound = errors.New("not found")

var ErrLeaseLost = errors.New("update job lease lost")

var ErrSuperseded = errors.New("update job superseded")

var ErrThreadDraftState = errors.New("invalid thread draft state")

var ErrThreadBriefState = errors.New("invalid thread brief state")

// Package threads provides the small publishing boundary used to create and
// publish text posts and replies through Meta's Threads API.
package threads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

const MaxTextRunes = 500

// Publisher is the application-facing Threads publishing contract. Creating a
// container and publishing it are deliberately separate operations so callers
// can recover safely when the outcome of the publish request is unknown.
type Publisher interface {
	Enabled() bool
	CreateText(ctx context.Context, text, replyToID string) (containerID string, err error)
	ContainerStatus(ctx context.Context, id string) (Status, error)
	Publish(ctx context.Context, containerID string) (Publication, error)
}

type ContainerState string

const (
	StateExpired    ContainerState = "EXPIRED"
	StateError      ContainerState = "ERROR"
	StateFinished   ContainerState = "FINISHED"
	StateInProgress ContainerState = "IN_PROGRESS"
	StatePublished  ContainerState = "PUBLISHED"
)

type Status struct {
	ID           string
	State        ContainerState
	ErrorMessage string
}

func (s Status) Ready() bool {
	return s.State == StateFinished
}

type Publication struct {
	ID        string
	Permalink string
}

// FailureClass describes whether a failed write is known not to have taken
// effect. An ambiguous publish must be reconciled through ContainerStatus
// before the caller considers creating or publishing another container.
type FailureClass string

const (
	Definite  FailureClass = "definite"
	Ambiguous FailureClass = "ambiguous"
)

type Code string

const (
	CodeUnknown          Code = "unknown"
	CodeConfig           Code = "config"
	CodeDisabled         Code = "disabled"
	CodeInvalidInput     Code = "invalid_input"
	CodeAuth             Code = "auth"
	CodePermission       Code = "permission"
	CodeRateLimited      Code = "rate_limited"
	CodeTransport        Code = "transport"
	CodeUpstream         Code = "upstream"
	CodeRedirect         Code = "redirect"
	CodeResponseTooLarge Code = "response_too_large"
	CodeInvalidResponse  Code = "invalid_response"
	CodeNotFound         Code = "not_found"
)

var (
	ErrDisabled  = errors.New("threads publisher disabled")
	ErrTransport = errors.New("threads transport failure")
)

// Error contains only bounded, non-sensitive diagnostics. It intentionally
// does not retain Meta's message field, the request URL, form body, post text,
// or access token because upstream errors can reflect request data.
type Error struct {
	Operation    string
	Code         Code
	Class        FailureClass
	HTTPStatus   int
	GraphCode    int
	GraphSubcode int
	TraceID      string
	cause        error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	operation := e.Operation
	if operation == "" {
		operation = "request"
	}
	code := e.Code
	if code == "" {
		code = CodeUnknown
	}
	class := e.Class
	if class == "" {
		class = Definite
	}
	return fmt.Sprintf("threads %s failed: %s (%s)", operation, code, class)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func IsAmbiguous(err error) bool {
	var threadsErr *Error
	return errors.As(err, &threadsErr) && threadsErr.Class == Ambiguous
}

// SafeCode returns a stable code suitable for metrics, logs, and user-safe
// branching. It never derives its result from an upstream error string.
func SafeCode(err error) string {
	if err == nil {
		return ""
	}
	var threadsErr *Error
	if errors.As(err, &threadsErr) && threadsErr.Code != "" {
		return string(threadsErr.Code)
	}
	if errors.Is(err, ErrDisabled) {
		return string(CodeDisabled)
	}
	return string(CodeUnknown)
}

// Disabled is a fail-closed Publisher for installations where Threads has not
// been configured.
type Disabled struct{}

func NewDisabled() *Disabled {
	return &Disabled{}
}

func (Disabled) Enabled() bool {
	return false
}

func (Disabled) CreateText(context.Context, string, string) (string, error) {
	return "", disabledError("create_text")
}

func (Disabled) ContainerStatus(context.Context, string) (Status, error) {
	return Status{}, disabledError("container_status")
}

func (Disabled) Publish(context.Context, string) (Publication, error) {
	return Publication{}, disabledError("publish")
}

func disabledError(operation string) error {
	return &Error{
		Operation: operation,
		Code:      CodeDisabled,
		Class:     Definite,
		cause:     ErrDisabled,
	}
}

// Fake is a deterministic in-memory Publisher. IDs depend only on call order,
// making it suitable for local development and repeatable service tests.
type Fake struct {
	mu         sync.Mutex
	nextID     uint64
	containers map[string]*fakeContainer
}

type fakeContainer struct {
	publicationID string
}

func NewFake() *Fake {
	return &Fake{containers: make(map[string]*fakeContainer)}
}

func (*Fake) Enabled() bool {
	return true
}

func (f *Fake) CreateText(_ context.Context, text, replyToID string) (string, error) {
	if err := validateText(text, "create_text"); err != nil {
		return "", err
	}
	if err := validateOptionalID(replyToID, "create_text"); err != nil {
		return "", err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInitialized()
	f.nextID++
	id := fmt.Sprintf("fake-container-%06d", f.nextID)
	f.containers[id] = &fakeContainer{}
	return id, nil
}

func (f *Fake) ContainerStatus(_ context.Context, id string) (Status, error) {
	if err := validateRequiredID(id, "container_status"); err != nil {
		return Status{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInitialized()
	container, ok := f.containers[strings.TrimSpace(id)]
	if !ok {
		return Status{}, &Error{Operation: "container_status", Code: CodeNotFound, Class: Definite}
	}
	state := StateFinished
	if container.publicationID != "" {
		state = StatePublished
	}
	return Status{ID: strings.TrimSpace(id), State: state}, nil
}

func (f *Fake) Publish(_ context.Context, containerID string) (Publication, error) {
	if err := validateRequiredID(containerID, "publish"); err != nil {
		return Publication{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureInitialized()
	containerID = strings.TrimSpace(containerID)
	container, ok := f.containers[containerID]
	if !ok {
		return Publication{}, &Error{Operation: "publish", Code: CodeNotFound, Class: Definite}
	}
	if container.publicationID == "" {
		container.publicationID = "fake-publication-" + strings.TrimPrefix(containerID, "fake-container-")
	}
	return Publication{ID: container.publicationID}, nil
}

func (f *Fake) ensureInitialized() {
	if f.containers == nil {
		f.containers = make(map[string]*fakeContainer)
	}
}

func validateText(text, operation string) error {
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
		return &Error{Operation: operation, Code: CodeInvalidInput, Class: Definite}
	}
	if utf8.RuneCountInString(text) > MaxTextRunes {
		return &Error{Operation: operation, Code: CodeInvalidInput, Class: Definite}
	}
	return nil
}

func validateOptionalID(id, operation string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	return validateRequiredID(id, operation)
}

func validateRequiredID(id, operation string) error {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 256 {
		return &Error{Operation: operation, Code: CodeInvalidInput, Class: Definite}
	}
	for _, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' || char == ':' {
			continue
		}
		return &Error{Operation: operation, Code: CodeInvalidInput, Class: Definite}
	}
	return nil
}

var (
	_ Publisher = (*Disabled)(nil)
	_ Publisher = Disabled{}
	_ Publisher = (*Fake)(nil)
)

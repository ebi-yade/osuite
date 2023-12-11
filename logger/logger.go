package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"time"

	"cloud.google.com/go/logging"
	"github.com/google/uuid"
)

var (
	LevelDefault   = slog.Level(logging.Default)
	LevelDebug     = slog.Level(logging.Debug)
	LevelInfo      = slog.Level(logging.Info)
	LevelNotice    = slog.Level(logging.Notice)
	LevelWarning   = slog.Level(logging.Warning)
	LevelError     = slog.Level(logging.Error)
	LevelCritical  = slog.Level(logging.Critical)
	LevelAlert     = slog.Level(logging.Alert)
	LevelEmergency = slog.Level(logging.Emergency)

	logAttrReporting = slog.String(
		"@type",
		"type.googleapis.com/google.devtools.clouderrorreporting.v1beta1.ReportedErrorEvent",
	)
)

const (
	logMessageKey        = "message"
	logSeverityKey       = "severity"
	logSourceLocationKey = "logging.googleapis.com/sourceLocation"
	logTraceKey          = "logging.googleapis.com/trace"
	logSpanIDKey         = "logging.googleapis.com/spanId"
	logInsertIDKey       = "logging.googleapis.com/insertId"
)

type Logger struct {
	handler   slog.Handler
	projectID string

	// dependency injection
	printErr   func(error) string
	getTraceID func(context.Context) string
	getSpanID  func(context.Context) string
}

type LoggerOption func(*Logger)

// WithPrintError sets the function to print error.
func WithPrintError(f func(error) string) LoggerOption {
	return func(l *Logger) {
		l.printErr = f
	}
}

// WithTraceID sets the function to get traceID from context.
func WithTraceID(f func(context.Context) string) LoggerOption {
	return func(l *Logger) {
		l.getTraceID = f
	}
}

// WithSpanID sets the function to get spanID from context.
func WithSpanID(f func(context.Context) string) LoggerOption {
	return func(l *Logger) {
		l.getSpanID = f
	}
}

func New(w io.Writer, projectID string, minLevel slog.Level, opts ...LoggerOption) *Logger {
	replaceAttr := func(groups []string, a slog.Attr) slog.Attr {
		switch a.Key {
		case slog.LevelKey:
			return slog.String(logSeverityKey, logging.Severity(a.Value.Any().(slog.Level)).String())
		case slog.SourceKey:
			a.Key = logSourceLocationKey
		case slog.MessageKey:
			a.Key = logMessageKey
		}
		return a
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{AddSource: true, Level: minLevel, ReplaceAttr: replaceAttr})

	// default
	logger := &Logger{
		handler:   handler,
		projectID: projectID,
		printErr: func(err error) string {
			return fmt.Sprintf("%+v", err) // expected errors are wrapped by pkg/errors
		},
		getTraceID: func(ctx context.Context) string {
			return ""
		},
		getSpanID: func(ctx context.Context) string {
			return ""
		},
	}

	for _, apply := range opts {
		apply(logger)
	}
	return logger
}

type EntryOption func(*EntryParams)

type EntryParams struct {
	level       slog.Level
	msg         string
	attrs       []slog.Attr
	skipCaller  int
	errorReport bool
}

func NewEntryParams(level slog.Level, msg string, opts ...EntryOption) EntryParams {
	params := EntryParams{
		level: level,
		msg:   msg,
	}
	for _, apply := range opts {
		apply(&params)
	}
	return params
}

// WithAttrs sets the attributes of the entry.
func WithAttrs(attrs ...slog.Attr) EntryOption {
	return func(o *EntryParams) {
		o.attrs = attrs
	}
}

// WithSkipCaller sets the number of stack frames to skip when getting the caller.
func WithSkipCaller(skip int) EntryOption {
	return func(o *EntryParams) {
		o.skipCaller = skip
	}
}

// WithErrorReport sets whether the entry should be reported as an error.
func WithErrorReport(report bool) EntryOption {
	return func(o *EntryParams) {
		o.errorReport = report
	}
}

// Design note:
// The write method is the only method to output the log entry.
// And we keep it called by user's code with just one level of wrapping.
func (l *Logger) write(ctx context.Context, params EntryParams) {
	if !l.handler.Enabled(ctx, params.level) {
		return
	}

	//params := &EntryParams{}
	//for _, apply := range opts {
	//	apply(params)
	//}

	// generate information to ensure the uniqueness of the entry
	now := time.Now()
	insertId := uuid.NewString()

	// 0: runtime.Callers, 1: Logger.write, 2: Logger.<Exported Method>, 3: <Your Code>
	const defaultSkipCaller = 3
	pcs := [1]uintptr{}
	runtime.Callers(defaultSkipCaller+params.skipCaller, pcs[:])
	r := slog.NewRecord(now, params.level, params.msg, pcs[0])

	attrs := []slog.Attr{
		slog.String(logInsertIDKey, insertId),
	}
	if params.errorReport {
		attrs = append(attrs, logAttrReporting)
	}
	if traceID := l.getTraceID(ctx); traceID != "" {
		attrs = append(attrs, slog.String(logTraceKey, fmt.Sprintf("projects/%s/traces/%s", l.projectID, traceID)))
		if spanID := l.getSpanID(ctx); spanID != "" {
			attrs = append(attrs, slog.String(logSpanIDKey, spanID))
		}
	}
	attrs = append(attrs, params.attrs...)
	r.AddAttrs(attrs...)

	// It is safe to retry because the uniqueness of the entry is guaranteed by time and insertId.
	// TODO: consider to use some kind of retry strategy
	l.handler.Handle(ctx, r)
}

func (l *Logger) Default(ctx context.Context, msg string, opts ...EntryOption) {
	l.write(ctx, NewEntryParams(LevelDefault, msg, opts...))
}

func (l *Logger) Debug(ctx context.Context, msg string, opts ...EntryOption) {
	l.write(ctx, NewEntryParams(LevelDebug, msg, opts...))
}

func (l *Logger) Info(ctx context.Context, msg string, opts ...EntryOption) {
	l.write(ctx, NewEntryParams(LevelInfo, msg, opts...))
}

func (l *Logger) Notice(ctx context.Context, msg string, opts ...EntryOption) {
	l.write(ctx, NewEntryParams(LevelNotice, msg, opts...))
}

func (l *Logger) Warn(ctx context.Context, msg string, opts ...EntryOption) {
	l.write(ctx, NewEntryParams(LevelWarning, msg, opts...))
}

func (l *Logger) Error(ctx context.Context, err error, opts ...EntryOption) {
	params := NewEntryParams(LevelError, l.printErr(err), opts...)
	params.errorReport = true
	l.write(ctx, params)
}

func (l *Logger) Critical(ctx context.Context, err error, opts ...EntryOption) {
	params := NewEntryParams(LevelCritical, l.printErr(err), opts...)
	params.errorReport = true
	l.write(ctx, params)
}

func (l *Logger) Alert(ctx context.Context, err error, opts ...EntryOption) {
	params := NewEntryParams(LevelAlert, l.printErr(err), opts...)
	params.errorReport = true
	l.write(ctx, params)
}

func (l *Logger) Emergency(ctx context.Context, err error, opts ...EntryOption) {
	params := NewEntryParams(LevelEmergency, l.printErr(err), opts...)
	params.errorReport = true
	l.write(ctx, params)
}

// Custom provides you a way to write a log entry with high flexibility,
// but we will not make an effort to keep the backward compatibility of this method.
// We recommend you to implement your own logger when you want to use this method.
func (l *Logger) Custom(ctx context.Context, params EntryParams) {
	l.write(ctx, params)
}

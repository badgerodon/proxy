package proxy

import (
	l "log"
	"os"
)

var DefaultLogger *l.Logger

func init() {
	DefaultLogger = l.New(os.Stderr, "", l.Lshortfile)
}

func log(format string, args ...interface{}) {
	DefaultLogger.Printf(format, args...)
}

func info(format string, args ...interface{}) {
	log(format+"\n", args...)
}

func warn(format string, args ...interface{}) {
	warn(format+"\n", args...)
}

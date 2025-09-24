package panicutil

import (
	"fmt"
	"os"
	"runtime/debug"

	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"
)

func HandlePanic(name string) {
	if r := recover(); r != nil {

		stack := debug.Stack()
		msg := fmt.Sprintf("PANIC recovered in %s: %v\nStacktrace:\n%s", name, r, string(stack))

		klog.Errorf("%s", msg)
		fmt.Fprintln(os.Stderr, msg)

		logs.FlushLogs()

		os.Exit(1)
	}
}

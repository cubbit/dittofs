// Package scenarios holds the demo's stories, one file each, registered
// here: the interactive driver's script, minus its machinery.
package scenarios

import "github.com/cubbit/dittofs/cmd/jdemo/jdemo"

func init() {
	jdemo.RegisterStory("basic", BasicStory)
	jdemo.RegisterStory("crash", CrashStory)
	jdemo.RegisterStory("limits", LimitsStory)
}

// Names lists the registered stories, the driver's unknown-story message.
func Names() []string {
	return jdemo.SortedStoryNames()
}

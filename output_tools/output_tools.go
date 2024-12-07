package outputtools

import (
	"fmt"
)

type OutputColor int32

const (
	Reset   OutputColor = 0
	Red     OutputColor = 31
	Green   OutputColor = 32
	Yellow  OutputColor = 33
	Blue    OutputColor = 34
	Magenta OutputColor = 35
	Cyan    OutputColor = 36
	Gray    OutputColor = 37
	White   OutputColor = 97
)

func PrintlnColored(text string, color OutputColor) {
	colored := fmt.Sprintf("\x1b[%dm%s\x1b[0m", color, text)
	fmt.Println(colored)
}

package version

import "fmt"

const (
	Base = "0.8.4"
)

var (
	gittag = ""
)

func Version() string {
	if gittag == "" {
		return fmt.Sprintf("%s-dev", Base)
	}
	return Base
}

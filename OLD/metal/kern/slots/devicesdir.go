package slots

import (
	"errors"
	"strings"
)

// De namespace /devices bestaat in élke smaak, ook waar er niets in kan
// hangen: een pad daaronder raakt het volume NOOIT (devices.go). Buiten de
// media-smaak is hij leeg en zegt elke naam ErrNoDevice — zo kan er ook op een
// headless- of gui-node geen echte map /devices op het volume ontstaan die
// later, na een flip naar media, een apparaat verstopt.

// DevicesDir is de namespace zelf.
const DevicesDir = "/devices"

// ErrNoDevice is wat een taak krijgt voor een naam die er niet is. Eigen fout,
// want dit is geen storing van de node: er hangt gewoon niets (meer) aan.
var ErrNoDevice = errors.New("no such device on this node")

// isDevicePath zegt of een opgelost pad in onze namespace ligt. De map zelf
// telt mee: die is te listen.
func isDevicePath(p string) bool {
	return p == DevicesDir || strings.HasPrefix(p, DevicesDir+"/")
}

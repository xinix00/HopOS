module hop-os/metal

go 1.26.4

require (
	github.com/usbarmory/go-net v0.0.0-20260626130943-dad9ef39fd9b
	github.com/usbarmory/tamago v1.26.4
)

require (
	github.com/google/btree v1.1.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/soypat/lneto v0.1.1-0.20260609173350-82f946154800 // indirect
	github.com/xinix00/hoplock v0.0.0-00010101000000-000000000000 // indirect
	github.com/xinix00/hoplockserver v0.0.0-00010101000000-000000000000 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/time v0.7.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

require (
	gvisor.dev/gvisor v0.0.0-20250911055229-61a46406f068
	hop v0.0.0
)

// hop + zijn lokale replaces (replace-directives gelden niet transitief).
replace (
	github.com/xinix00/hoplock => /Users/derek/haaslock
	github.com/xinix00/hoplockserver => /Users/derek/Git/easy/hoplockserver
	hop => /Users/derek/Git/easy/hop
)


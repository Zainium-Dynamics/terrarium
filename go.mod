module github.com/Zainium-Dynamics/terrarium

go 1.26.8

require (
	github.com/Zainium-Dynamics/zainium-installer v0.0.0
	golang.org/x/sys v0.46.0
)

// until the installer's pkg/ layout is published, build against the local checkout
replace github.com/Zainium-Dynamics/zainium-installer => ../zainium_installer

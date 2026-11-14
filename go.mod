module github.com/Zainium-Dynamics/terrarium

go 1.26.8

require (
	github.com/Zainium-Dynamics/zainium-installer v0.0.0-20261114195442-c4d4a02ab08d
	golang.org/x/sys v0.46.0
)

// the installer's repository is zainium_installer; its module is named zainium-installer
replace github.com/Zainium-Dynamics/zainium-installer => github.com/Zainium-Dynamics/zainium_installer v0.0.0-20261114195442-c4d4a02ab08d

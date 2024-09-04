package vcd

// this could be done much better with cloud-init for Linux,
// and cloud-base for Windows, but this is a quick and dirty way.
const (
	linuxGuestCustomizationScript = `#!/bin/bash
if [ x$1 == x"precustomization" ]; then
	echo 'Precustom'
elif [ x$1 == x"postcustomization" ]; then
	mkdir -p /root/.ssh
	echo '{{.PublicKey}}' >> /root/.ssh/authorized_keys
	chmod -R go-rwx /root/.ssh
fi`

	windowsGuestCustomizationScript = `@echo off
if "%1" == "postcustomization" (
	echo {{.PublicKey}} > C:\ProgramData\ssh\administrators_authorized_keys
)`
)

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
if "%1" == "precustomization" (
	echo Performing precustomization tasks
) else if "%1" == "postcustomization" (
	echo Performing postcustomization tasks
	
	REM Create the .ssh directory if it doesn't exist
	if not exist C:\Users\administrator\.ssh mkdir C:\Users\administrator\.ssh

	REM Add SSH public key
	echo {{.PublicKey}} > C:\Users\administrator\.ssh\authorized_keys
	
	REM Set appropriate permissions on the authorized_keys file
	icacls C:\Users\administrator\.ssh\authorized_keys /inheritance:r /grant "Administrators:F" /grant "SYSTEM:F"	
	
	REM Restart the SSH service
	net stop sshd
	net start sshd
	
	REM Ensure the SSH service starts automatically
	sc config sshd start= auto
	
	echo Postcustomization tasks completed
)
`
)

#!/bin/sh -e
# starts kubefwd in the background
log="/var/log/port-forward.log"

echo "killing kubefwd"
pkill kubefwd || true

echo "starting kubefwd in background"
nohup kubefwd svc --all-namespaces >> "${log}" &

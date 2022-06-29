#! /bin/bash

PODIP=$(kubectl get pods -n sciencedata-dev user-pods-backend-testing -o json | jq -r '.status.podIP')
sed "s/PODIP/$PODIP/" Caddyfile-backend > /tmp/Caddyfile-backend
caddy start -config /tmp/Caddyfile-backend &>> "/var/log/caddy/backend.log" &

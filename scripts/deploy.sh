#!/usr/bin/env bash
# Cross-compile the server and ship it to the host configured in .deploy.env.
# Deploys with the operator's own gcloud credentials — no keys live in the repo.
#
# Required (set in .deploy.env or the environment):
#   DEPLOY_INSTANCE  gcloud compute instance name
#   DEPLOY_ZONE      its zone
#   DEPLOY_USER      ssh user on the instance (needs passwordless sudo)
# Optional:
#   DEPLOY_PATH      install dir on the host    (default /opt/submission-triage)
#   DEPLOY_SERVICE   systemd unit + binary name (default submission-triage)
set -euo pipefail

if [ -f .deploy.env ]; then . ./.deploy.env; fi

: "${DEPLOY_INSTANCE:?set DEPLOY_INSTANCE — see .deploy.env.example}"
: "${DEPLOY_ZONE:?set DEPLOY_ZONE}"
: "${DEPLOY_USER:?set DEPLOY_USER}"
DEPLOY_PATH="${DEPLOY_PATH:-/opt/submission-triage}"
DEPLOY_SERVICE="${DEPLOY_SERVICE:-submission-triage}"

bin="bin/server-linux-amd64"
echo "building $bin ..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$bin" ./cmd/server

target="$DEPLOY_USER@$DEPLOY_INSTANCE"
echo "shipping to $target ($DEPLOY_ZONE) ..."
gcloud compute scp "$bin" "$target:/tmp/st-new" --zone "$DEPLOY_ZONE" --quiet

gcloud compute ssh "$target" --zone "$DEPLOY_ZONE" --quiet --command "
  set -e
  sudo install -m 0755 /tmp/st-new $DEPLOY_PATH/$DEPLOY_SERVICE.new
  sudo cp -p $DEPLOY_PATH/$DEPLOY_SERVICE $DEPLOY_PATH/$DEPLOY_SERVICE.bak
  sudo systemctl stop $DEPLOY_SERVICE
  sudo mv $DEPLOY_PATH/$DEPLOY_SERVICE.new $DEPLOY_PATH/$DEPLOY_SERVICE
  sudo systemctl start $DEPLOY_SERVICE
  rm -f /tmp/st-new
  sleep 2
  systemctl is-active $DEPLOY_SERVICE"

echo "deployed: $DEPLOY_SERVICE on $DEPLOY_INSTANCE"

# katalog-sync [![Build Status](https://travis-ci.org/wish/katalog-sync.svg?branch=master)](https://travis-ci.org/wish/katalog-sync)  [![Go Report Card](https://goreportcard.com/badge/github.com/wish/katalog-sync)](https://goreportcard.com/report/github.com/wish/katalog-sync)  [![Docker Repository on Quay](https://quay.io/repository/wish/katalog-sync/status "Docker Repository on Quay")](https://quay.io/repository/wish/katalog-sync)

katalog-sync is a node-local mechanism for syncing k8s pods to consul services.

katalog-sync has:

- node-local syncing to local consul-agent
- agent-services in consul, meaning health of those endpoints is tied to the node agent
- sync readiness state from k8s as check to consul
- (optional) sidecar service to ensure consul registration before a pod is marked "ready"

katalog-sync does this by making a few assumptions:

- You have a consul-agent running on each node (presumably as a Daemonset)
- You want to sync Pods to consul services and have the readiness values reflected
- Your pods can communicate with Daemonsets running on the same node

# kube-oom-watcher

Watches Kubernetes API server for OOMKilling events and sends pod name and namespace message to Slack/Discord.

## Requirements

* Installed [ps-report](https://github.com/smpio/ps-report) on all cluster nodes

## How it works

1. Get OOMKilling event
2. Find matching process in `ps-report` database
3. Extract pod uid from cgroup
4. Find pod by uid
5. Send pod info to Slack/Discord web hook

## Usage

```
TODO
```

#!/usr/bin/env bash

IMAGE=gcr.io/k8s-workshop-213617/k8s-slack-inviter:latest


GOOS=linux GOARCH=amd64 go build main.go


docker build . -t ${IMAGE}
docker push ${IMAGE}


OLD_POD=$(kubectl get pods -n slack-inviter -o jsonpath="{.items[0].metadata.name}")
kubectl delete pod $OLD_POD -n slack-inviter

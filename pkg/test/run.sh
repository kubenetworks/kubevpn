#!/bin/bash

export KUBECONFIG=~/.kube/vke
export NS=kube-system
kubectl apply -f pod.yaml -n $NS
kubectl wait --for=condition=Ready pod/test -n $NS
cd ./server && GOARCH=amd64 GOOS=linux go build -o main
kubectl cp main test:/app/main -n $NS
rm -fr main
kubectl port-forward pods/test 1080 -n $NS

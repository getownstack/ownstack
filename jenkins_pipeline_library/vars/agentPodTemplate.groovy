// The agent pod. The docker container uses the host's Docker Engine through
// its socket, so keep its CLI on the Engine's major version: Engine 29
// rejects the API that older CLIs such as 24 speak.
def call() {
    return '''\
apiVersion: v1
kind: Pod
spec:
  volumes:
    - name: dockersock
      hostPath:
        path: /var/run/docker.sock
        type: Socket
  containers:
    - name: docker
      image: docker:29-cli
      command: [cat]
      tty: true
      volumeMounts:
        - name: dockersock
          mountPath: /var/run/docker.sock
    - name: helm
      image: dtzar/helm-kubectl:4.3.0
      command: [cat]
      tty: true
'''
}

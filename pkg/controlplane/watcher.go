package controlplane

import (
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

type OperationType int

const (
	Create OperationType = iota
	Remove
	Modify
)

type NotifyMessage struct {
	Operation OperationType
	FilePath  string
}

func Watch(watcher *fsnotify.Watcher, filename string, notifyCh chan<- NotifyMessage) error {
	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return errors.Errorf("watcher has closed")
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				notifyCh <- NotifyMessage{
					Operation: Modify,
					FilePath:  event.Name,
				}
			} else if event.Op&fsnotify.Create == fsnotify.Create {
				notifyCh <- NotifyMessage{
					Operation: Create,
					FilePath:  event.Name,
				}
			} else if event.Op&fsnotify.Remove == fsnotify.Remove {
				notifyCh <- NotifyMessage{
					Operation: Remove,
					FilePath:  event.Name,
				}
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return errors.Errorf("watcher error closed")
			}
			return err

		case <-ticker.C:
			notifyCh <- NotifyMessage{
				Operation: Modify,
				FilePath:  filename,
			}
		}
	}
}

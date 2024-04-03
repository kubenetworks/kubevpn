package cp

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/cmd/exec"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// CopyOptions have the data required to perform the copy operation
type CopyOptions struct {
	Container  string
	Namespace  string
	NoPreserve bool
	MaxTries   int

	ClientConfig      *restclient.Config
	Clientset         kubernetes.Interface
	ExecParentCmdName string

	args []string

	genericiooptions.IOStreams
}

// NewCopyOptions creates the options for copy
func NewCopyOptions(ioStreams genericiooptions.IOStreams) *CopyOptions {
	return &CopyOptions{
		IOStreams: ioStreams,
	}
}

var (
	errFileSpecDoesntMatchFormat = errors.New("filespec must match the canonical format: [[namespace/]pod:]file/path")
)

func extractFileSpec(arg string) (fileSpec, error) {
	i := strings.Index(arg, ":")

	// filespec starting with a semicolon is invalid
	if i == 0 {
		return fileSpec{}, errFileSpecDoesntMatchFormat
	}

	// C:\Users\ADMINI~1\AppData\Local\Temp\849198392506502457
	// disk name C is not a pod name
	if i == -1 || (runtime.GOOS == "windows" && strings.Contains("ABCDEFGHIJKLMNOPQRSTUVWXYZ", arg[:i])) {
		return fileSpec{
			File: newLocalPath(arg),
		}, nil
	}

	pod, file := arg[:i], arg[i+1:]
	pieces := strings.Split(pod, "/")
	switch len(pieces) {
	case 1:
		return fileSpec{
			PodName: pieces[0],
			File:    newRemotePath(file),
		}, nil
	case 2:
		return fileSpec{
			PodNamespace: pieces[0],
			PodName:      pieces[1],
			File:         newRemotePath(file),
		}, nil
	default:
		return fileSpec{}, errFileSpecDoesntMatchFormat
	}
}

// Complete completes all the required options
func (o *CopyOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	if cmd.Parent() != nil {
		o.ExecParentCmdName = cmd.Parent().CommandPath()
	}

	var err error
	o.Namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.Clientset, err = f.KubernetesClientSet()
	if err != nil {
		return err
	}

	o.ClientConfig, err = f.ToRESTConfig()
	if err != nil {
		return err
	}

	o.args = args
	return nil
}

// Validate makes sure provided values for CopyOptions are valid
func (o *CopyOptions) Validate() error {
	if len(o.args) != 2 {
		return fmt.Errorf("source and destination are required")
	}
	return nil
}

// Run performs the execution
func (o *CopyOptions) Run() error {
	srcSpec, err := extractFileSpec(o.args[0])
	if err != nil {
		return err
	}
	destSpec, err := extractFileSpec(o.args[1])
	if err != nil {
		return err
	}

	if len(srcSpec.PodName) != 0 && len(destSpec.PodName) != 0 {
		return fmt.Errorf("one of src or dest must be a local file specification")
	}
	if len(srcSpec.File.String()) == 0 || len(destSpec.File.String()) == 0 {
		return errors.New("filepath can not be empty")
	}

	if len(srcSpec.PodName) != 0 {
		return o.copyFromPod(srcSpec, destSpec)
	}
	if len(destSpec.PodName) != 0 {
		return o.copyToPod(srcSpec, destSpec, &exec.ExecOptions{})
	}
	return fmt.Errorf("one of src or dest must be a remote file specification")
}

// checkDestinationIsDir receives a destination fileSpec and
// determines if the provided destination path exists on the
// pod. If the destination path does not exist or is _not_ a
// directory, an error is returned with the exit code received.
func (o *CopyOptions) checkDestinationIsDir(dest fileSpec) error {
	options := &exec.ExecOptions{
		StreamOptions: exec.StreamOptions{
			IOStreams: genericiooptions.IOStreams{
				Out:    bytes.NewBuffer([]byte{}),
				ErrOut: bytes.NewBuffer([]byte{}),
			},

			Namespace: dest.PodNamespace,
			PodName:   dest.PodName,
		},

		Command:  []string{"test", "-d", dest.File.String()},
		Executor: &exec.DefaultRemoteExecutor{},
	}

	return o.execute(options)
}

func (o *CopyOptions) copyToPod(src, dest fileSpec, options *exec.ExecOptions) error {
	if _, err := os.Stat(src.File.String()); err != nil {
		return fmt.Errorf("%s doesn't exist in local filesystem", src.File)
	}
	reader, writer := io.Pipe()

	srcFile := src.File.(localPath)
	destFile := dest.File.(remotePath)

	if err := o.checkDestinationIsDir(dest); err == nil {
		// If no error, dest.File was found to be a directory.
		// Copy specified src into it
		destFile = destFile.Join(srcFile.Base())
	}

	go func(src localPath, dest remotePath, writer io.WriteCloser) {
		defer writer.Close()
		if err := makeTar(src, dest, writer); err != nil {
			log.Errorf("Error making tar: %v", err)
		}
	}(srcFile, destFile, writer)
	var cmdArr []string

	if o.NoPreserve {
		cmdArr = []string{"tar", "--no-same-permissions", "--no-same-owner", "-xmfh", "-"}
	} else {
		cmdArr = []string{"tar", "-xmfh", "-"}
	}
	destFileDir := destFile.Dir().String()
	if len(destFileDir) > 0 {
		cmdArr = append(cmdArr, "-C", destFileDir)
	}

	options.StreamOptions = exec.StreamOptions{
		IOStreams: genericiooptions.IOStreams{
			In:     reader,
			Out:    o.Out,
			ErrOut: o.ErrOut,
		},
		Stdin: true,

		Namespace: dest.PodNamespace,
		PodName:   dest.PodName,
	}

	options.Command = cmdArr
	options.Executor = &exec.DefaultRemoteExecutor{}
	return o.execute(options)
}

func (o *CopyOptions) copyFromPod(src, dest fileSpec) error {
	reader := newTarPipe(src, o)
	srcFile := src.File.(remotePath)
	destFile := dest.File.(localPath)
	// remove extraneous path shortcuts - these could occur if a path contained extra "../"
	// and attempted to navigate beyond "/" in a remote filesystem
	prefix := stripPathShortcuts(srcFile.StripSlashes().Clean().String())
	return o.untarAll(prefix, destFile, reader)
}

type TarPipe struct {
	src       fileSpec
	o         *CopyOptions
	reader    *io.PipeReader
	outStream *io.PipeWriter
	bytesRead uint64
	retries   int
}

func newTarPipe(src fileSpec, o *CopyOptions) *TarPipe {
	t := new(TarPipe)
	t.src = src
	t.o = o
	t.initReadFrom(0)
	return t
}

func (t *TarPipe) initReadFrom(n uint64) {
	t.reader, t.outStream = io.Pipe()
	options := &exec.ExecOptions{
		StreamOptions: exec.StreamOptions{
			IOStreams: genericiooptions.IOStreams{
				In:     nil,
				Out:    t.outStream,
				ErrOut: t.o.Out,
			},

			Namespace: t.src.PodNamespace,
			PodName:   t.src.PodName,
		},

		Command:  []string{"tar", "cfh", "-", t.src.File.String()},
		Executor: &exec.DefaultRemoteExecutor{},
	}
	if t.o.MaxTries != 0 {
		options.Command = []string{"sh", "-c", fmt.Sprintf("tar cfh - %s | tail -c+%d", t.src.File, n)}
	}

	go func() {
		defer t.outStream.Close()
		if err := t.o.execute(options); err != nil {
			log.Errorf("Error executing command: %v", err)
		}
	}()
}

func (t *TarPipe) Read(p []byte) (n int, err error) {
	n, err = t.reader.Read(p)
	if err != nil {
		if t.o.MaxTries < 0 || t.retries < t.o.MaxTries {
			t.retries++
			fmt.Printf("Resuming copy at %d bytes, retry %d/%d\n", t.bytesRead, t.retries, t.o.MaxTries)
			t.initReadFrom(t.bytesRead + 1)
			err = nil
		} else {
			fmt.Printf("Dropping out copy after %d retries\n", t.retries)
		}
	} else {
		t.bytesRead += uint64(n)
	}
	return
}

func makeTar(src localPath, dest remotePath, writer io.Writer) error {
	// TODO: use compression here?
	tarWriter := tar.NewWriter(writer)
	defer tarWriter.Close()

	srcPath := src.Clean()
	destPath := dest.Clean()
	return recursiveTar(srcPath.Dir(), srcPath.Base(), destPath.Dir(), destPath.Base(), tarWriter)
}

func recursiveTar(srcDir, srcFile localPath, destDir, destFile remotePath, tw *tar.Writer) error {
	matchedPaths, err := srcDir.Join(srcFile).Glob()
	if err != nil {
		return err
	}
	for _, fpath := range matchedPaths {
		stat, err := os.Lstat(fpath)
		if err != nil {
			return err
		}
		if stat.IsDir() {
			files, err := os.ReadDir(fpath)
			if err != nil {
				return err
			}
			if len(files) == 0 {
				//case empty directory
				hdr, _ := tar.FileInfoHeader(stat, fpath)
				hdr.Name = destFile.String()
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
			}
			for _, f := range files {
				if err := recursiveTar(srcDir, srcFile.Join(newLocalPath(f.Name())),
					destDir, destFile.Join(newRemotePath(f.Name())), tw); err != nil {
					return err
				}
			}
			return nil
		} else if stat.Mode()&os.ModeSymlink != 0 {
			//case soft link
			hdr, _ := tar.FileInfoHeader(stat, fpath)
			target, err := os.Readlink(fpath)
			if err != nil {
				return err
			}

			hdr.Linkname = target
			hdr.Name = destFile.String()
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		} else {
			//case regular file or other file type like pipe
			hdr, err := tar.FileInfoHeader(stat, fpath)
			if err != nil {
				return err
			}
			hdr.Name = destFile.String()

			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}

			f, err := os.Open(fpath)
			if err != nil {
				return err
			}
			defer f.Close()

			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
			return f.Close()
		}
	}
	return nil
}

func (o *CopyOptions) untarAll(prefix string, dest localPath, reader io.Reader) error {
	// TODO: use compression here?
	tarReader := tar.NewReader(reader)
	var linkList []tar.Header
	var genDstFilename = func(headerName string) localPath {
		return dest.Join(newRemotePath(headerName[len(prefix):]))
	}
	for {
		header, err := tarReader.Next()
		if err != nil {
			if err != io.EOF {
				return err
			}
			break
		}

		// All the files will start with the prefix, which is the directory where
		// they were located on the pod, we need to strip down that prefix, but
		// if the prefix is missing it means the tar was tempered with.
		// For the case where prefix is empty we need to ensure that the path
		// is not absolute, which also indicates the tar file was tempered with.
		if !strings.HasPrefix(header.Name, prefix) {
			return fmt.Errorf("tar contents corrupted")
		}

		// header.Name is a name of the REMOTE file, so we need to create
		// a remotePath so that it goes through appropriate processing related
		// with cleaning remote paths
		destFileName := genDstFilename(header.Name)

		if !isRelative(dest, destFileName) {
			fmt.Fprintf(o.IOStreams.ErrOut, "warning: file %q is outside target destination, skipping\n", destFileName)
			continue
		}

		if err := os.MkdirAll(destFileName.Dir().String(), 0755); err != nil {
			return err
		}
		if header.FileInfo().IsDir() {
			if err := os.MkdirAll(destFileName.String(), 0755); err != nil {
				return err
			}
			continue
		}

		outFile, err := os.Create(destFileName.String())
		if err != nil {
			return err
		}
		defer outFile.Close()
		if _, err := io.Copy(outFile, tarReader); err != nil {
			return err
		}
		if err := outFile.Close(); err != nil {
			return err
		}

		// all file became into normal file, this means linkList to another file, do it later
		if header.Linkname != "" {
			linkList = append(linkList, *header)
		}
	}

	// handle linked file
	for _, f := range linkList {
		err := copyFromLink(linkList, f, genDstFilename)
		if err != nil {
			return err
		}
	}

	return nil
}

func (o *CopyOptions) execute(options *exec.ExecOptions) error {
	if len(options.Namespace) == 0 {
		options.Namespace = o.Namespace
	}

	if len(o.Container) > 0 {
		options.ContainerName = o.Container
	}

	options.Config = o.ClientConfig
	options.PodClient = o.Clientset.CoreV1()

	if err := options.Validate(); err != nil {
		return err
	}

	if err := options.Run(); err != nil {
		return err
	}
	return nil
}

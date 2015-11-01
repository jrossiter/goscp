package goscp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cheggaaa/pb"
	"golang.org/x/crypto/ssh"
)

var (
	// SCP messages
	fileCopyRx  = regexp.MustCompile(`C(?P<mode>\d{4}) (?P<length>\d+) (?P<filename>.+)`)
	dirCopyRx   = regexp.MustCompile(`D(?P<mode>\d{4}) (?P<length>\d+) (?P<dirname>.+)`)
	timestampRx = regexp.MustCompile(`T(?P<mtime>\d+) 0 (?P<atime>\d+) 0`)
	endDir      = "E"
)

type Client struct {
	SSHClient        *ssh.Client
	ProgressCallback func(out string)
	DestinationPath  []string

	// Errors that have occurred while communicating with host
	errors []error

	// Verbose output when communicating with host
	Verbose bool

	// Stop transfer on OS error - occurs during filepath.Walk
	StopOnOSError bool

	// Stdin for SSH session
	scpStdinPipe io.WriteCloser

	// Stdout for SSH session
	scpStdoutPipe *Reader
}

// Returns a ssh.Client wrapper.
// DestinationPath is set to the current directory by default.
func NewClient(c *ssh.Client) *Client {
	return &Client{
		SSHClient:       c,
		DestinationPath: []string{"."},
	}
}

// Set where content will be sent
func (c *Client) SetDestinationPath(path string) {
	c.DestinationPath = []string{path}
}

func (c *Client) addError(err error) {
	c.errors = append(c.errors, err)
}

// GetLastError should be queried after a call to Download() or Upload().
func (c *Client) GetLastError() error {
	if len(c.errors) > 0 {
		return c.errors[len(c.errors)-1]
	}
	return nil
}

// GetErrorStack returns all errors that have occurred so far
func (c *Client) GetErrorStack() []error {
	return c.errors
}

// Cancel an ongoing operation
func (c *Client) Cancel() {
	if c.scpStdoutPipe != nil {
		c.scpStdoutPipe.cancel <- struct{}{}
	}
}

// Download remotePath to c.DestinationPath
func (c *Client) Download(remotePath string) {
	session, err := c.SSHClient.NewSession()
	if err != nil {
		c.addError(err)
		return
	}
	defer session.Close()

	go func() {
		c.scpStdinPipe, err = session.StdinPipe()
		if err != nil {
			c.addError(err)
			return
		}
		defer c.scpStdinPipe.Close()

		r, err := session.StdoutPipe()
		if err != nil {
			c.addError(err)
			return
		}

		// Initialise transfer
		c.sendAck()

		// Wrapper to support cancellation
		c.scpStdoutPipe = &Reader{
			Reader: bufio.NewReader(r),
			cancel: make(chan struct{}, 1),
		}

		for {
			c.outputInfo("Reading message from source")
			msg, err := c.scpStdoutPipe.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					c.addError(err)
				}
				return
			}

			// Strip nulls and new lines
			msg = strings.TrimSpace(strings.Trim(msg, "\x00"))
			c.outputInfo(fmt.Sprintf("Received: %s", msg))

			// Confirm message
			c.sendAck()

			switch {
			case c.isFileCopyMsg(msg):
				// Handle incoming file
				err := c.file(msg)
				if err != nil {
					c.addError(err)
					return
				}
			case c.isDirCopyMsg(msg):
				// Handling incoming directory
				err := c.directory(msg)
				if err != nil {
					c.addError(err)
					return
				}
			case msg == endDir:
				// Directory finished, go up a directory
				c.upDirectory()
			case c.isWarningMsg(msg):
				c.addError(fmt.Errorf("Warning message: [%q]\n", msg))
				return
			case c.isErrorMsg(msg):
				c.addError(fmt.Errorf("Error message: [%q]\n", msg))
				return
			default:
				c.addError(fmt.Errorf("Unhandled message: [%q]\n", msg))
				return
			}

			// Confirm message
			c.sendAck()
		}
	}()

	cmd := fmt.Sprintf("scp -rf %s", remotePath)
	if err := session.Run(cmd); err != nil {
		c.addError(err)
		return
	}

	return
}

// Upload localPath to c.DestinationPath
func (c *Client) Upload(localPath string) {
	session, err := c.SSHClient.NewSession()
	if err != nil {
		c.addError(err)
		return
	}
	defer session.Close()

	go func() {
		c.scpStdinPipe, err = session.StdinPipe()
		if err != nil {
			c.addError(err)
			return
		}
		defer c.scpStdinPipe.Close()

		r, err := session.StdoutPipe()
		if err != nil {
			c.addError(err)
			return
		}

		// Wrapper to support cancellation
		c.scpStdoutPipe = &Reader{
			Reader: bufio.NewReader(r),
			cancel: make(chan struct{}, 1),
		}

		// This has already been used in the cmd call below
		// so it can be reused for 'end of directory' message handling
		c.DestinationPath = []string{}

		err = filepath.Walk(localPath, c.handleItem)
		if err != nil {
			c.addError(err)
			return
		}

		// End transfer
		paths := strings.Split(c.DestinationPath[0], "/")
		for range paths {
			c.sendEndOfDirectoryMessage()
		}
	}()

	cmd := fmt.Sprintf("scp -rt %s", filepath.Join(c.DestinationPath...))
	if err := session.Run(cmd); err != nil {
		c.addError(err)
		return
	}

	return
}

// Send an acknowledgement message
func (c *Client) sendAck() {
	fmt.Fprint(c.scpStdinPipe, "\x00")
}

// Send an error message
func (c *Client) sendErr() {
	fmt.Fprint(c.scpStdinPipe, "\x02")
}

// Check if an incoming message is a file copy message
func (c *Client) isFileCopyMsg(s string) bool {
	return strings.HasPrefix(s, "C")
}

// Check if an incoming message is a directory copy message
func (c *Client) isDirCopyMsg(s string) bool {
	return strings.HasPrefix(s, "D")
}

// Check if an incoming message is a warning
func (c *Client) isWarningMsg(s string) bool {
	return strings.HasPrefix(s, "\x01")
}

// Check if an incoming message is an error
func (c *Client) isErrorMsg(s string) bool {
	return strings.HasPrefix(s, "\x02")
}

// Send a directory message while in source mode
func (c *Client) sendDirectoryMessage(mode os.FileMode, dirname string) {
	msg := fmt.Sprintf("D0%o 0 %s", mode, dirname)
	fmt.Fprintln(c.scpStdinPipe, msg)
	c.outputInfo(fmt.Sprintf("Sent: %s", msg))
}

// Send a end of directory message while in source mode
func (c *Client) sendEndOfDirectoryMessage() {
	msg := endDir
	fmt.Fprintln(c.scpStdinPipe, msg)
	c.outputInfo(fmt.Sprintf("Sent: %s", msg))
}

// Send a file message while in source mode
func (c *Client) sendFileMessage(mode os.FileMode, size int64, filename string) {
	msg := fmt.Sprintf("C0%o %d %s", mode, size, filename)
	fmt.Fprintln(c.scpStdinPipe, msg)
	c.outputInfo(fmt.Sprintf("Sent: %s", msg))
}

// Handle directory copy message in sink mode
func (c *Client) directory(msg string) error {
	parts, err := c.parseMessage(msg, dirCopyRx)
	if err != nil {
		return err
	}

	err = os.Mkdir(filepath.Join(c.DestinationPath...)+string(filepath.Separator)+parts["dirname"], 0755)
	if err != nil {
		return err
	}

	// Traverse into directory
	c.DestinationPath = append(c.DestinationPath, parts["dirname"])

	return nil
}

// Handle file copy message in sink mode
func (c *Client) file(msg string) error {
	parts, err := c.parseMessage(msg, fileCopyRx)
	if err != nil {
		return err
	}

	fileLen, _ := strconv.Atoi(parts["length"])

	// Create local file
	localFile, err := os.Create(filepath.Join(c.DestinationPath...) + string(filepath.Separator) + parts["filename"])
	if err != nil {
		return err
	}
	defer localFile.Close()

	bar := c.NewProgressBar(fileLen)
	bar.Start()
	defer bar.Finish()

	mw := io.MultiWriter(localFile, bar)
	if n, err := io.CopyN(mw, c.scpStdoutPipe, int64(fileLen)); err != nil || n < int64(fileLen) {
		c.sendErr()
		return err
	}

	return nil
}

// Break down incoming protocol messages
func (c *Client) parseMessage(msg string, rx *regexp.Regexp) (map[string]string, error) {
	parts := make(map[string]string)
	matches := rx.FindStringSubmatch(msg)
	if len(matches) == 0 {
		return parts, errors.New("Could not parse protocol message: " + msg)
	}

	for i, name := range rx.SubexpNames() {
		parts[name] = matches[i]
	}
	return parts, nil
}

// Go back up one directory
func (c *Client) upDirectory() {
	c.DestinationPath = c.DestinationPath[:len(c.DestinationPath)-1]
}

// Handle each item coming through filepath.Walk
func (c *Client) handleItem(path string, info os.FileInfo, err error) error {
	if err != nil {
		// OS error
		c.outputInfo(fmt.Sprintf("Item error: %s", err))

		if c.StopOnOSError {
			return err
		}
		return nil
	}

	if info.IsDir() {
		// Handle directories
		if len(c.DestinationPath) != 0 {
			// If not first directory
			currentPath := strings.Split(c.DestinationPath[0], "/")
			newPath := strings.Split(path, "/")

			// <= slashes = going back up
			if len(newPath) <= len(currentPath) {
				// Send EOD messages for the amount of directories we go up
				for i := len(newPath) - 1; i < len(currentPath); i++ {
					c.sendEndOfDirectoryMessage()
				}
			}
		}
		c.DestinationPath = []string{path}
		c.sendDirectoryMessage(0644, filepath.Base(path))
	} else {
		// Handle regular files
		targetItem, err := os.Open(path)
		if err != nil {
			return err
		}

		c.sendFileMessage(0644, info.Size(), filepath.Base(path))

		if info.Size() > 0 {
			bar := c.NewProgressBar(int(info.Size()))
			bar.Start()
			defer bar.Finish()

			mw := io.MultiWriter(c.scpStdinPipe, bar)

			c.outputInfo(fmt.Sprintf("Sending file: %s", path))
			if _, err := io.Copy(mw, targetItem); err != nil {
				c.sendErr()
				return err
			}

			c.sendAck()
		} else {
			c.outputInfo(fmt.Sprintf("Sending empty file: %s", path))
			c.sendAck()
		}
	}

	return nil
}

func (c *Client) outputInfo(s ...string) {
	if c.Verbose {
		log.Println(s)
	}
}

// Create progress bar
func (c *Client) NewProgressBar(fileLength int) *pb.ProgressBar {
	bar := pb.New(fileLength)
	bar.Callback = c.ProgressCallback
	bar.ShowSpeed = true
	bar.ShowTimeLeft = true
	bar.ShowCounters = true
	bar.Units = pb.U_BYTES
	bar.SetRefreshRate(time.Second)
	bar.SetWidth(80)
	bar.SetMaxWidth(80)

	return bar
}

// Wrapper to support cancellation
type Reader struct {
	*bufio.Reader

	// Cancel an ongoing transfer
	cancel chan struct{}
}

// Additional cancellation check
func (r *Reader) Read(p []byte) (n int, err error) {
	select {
	case <-r.cancel:
		return 0, errors.New("Transfer cancelled")
	default:
		return r.Reader.Read(p)
	}
}

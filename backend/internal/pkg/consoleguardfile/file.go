package consoleguardfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const DefaultPath = "/app/data/console-degraded-egress-nodes.txt"

var fileMu sync.Mutex

// AppendUnique appends one non-empty proxy address unless the file already contains it.
func AppendUnique(path, value string) error {
	path = strings.TrimSpace(path)
	value = strings.TrimSpace(value)
	if path == "" {
		return errors.New("代理节点文件路径为空")
	}
	if value == "" {
		return errors.New("代理节点地址为空")
	}

	fileMu.Lock()
	defer fileMu.Unlock()

	content, err := read(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == value {
			return nil
		}
	}

	if directory := filepath.Dir(path); directory != "." {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if len(content) > 0 && content[len(content)-1] != '\n' {
		if _, err := io.WriteString(file, "\n"); err != nil {
			return err
		}
	}
	_, err = io.WriteString(file, value+"\n")
	return err
}

// Clear truncates the shared file while keeping the file itself available.
func Clear(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("代理节点文件路径为空")
	}

	fileMu.Lock()
	defer fileMu.Unlock()

	if directory := filepath.Dir(path); directory != "." {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Chmod(0o600)
}

// Read returns the current file content. A missing file is an empty file.
func Read(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("代理节点文件路径为空")
	}
	fileMu.Lock()
	defer fileMu.Unlock()
	content, err := read(path)
	return string(content), err
}

func read(path string) ([]byte, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return content, err
}

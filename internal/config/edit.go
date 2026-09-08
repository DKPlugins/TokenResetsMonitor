package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/DKPlugins/TokenResetsMonitor/internal/fileio"
	"gopkg.in/yaml.v3"
)

// SaveChannel updates only the chosen section and version. Values must be the
// unresolved configuration (including environment references), not Load's result.
func SaveChannel(path, channel string, value any, expected []byte) error {
	if channel != "telegram" && channel != "slack" {
		return errors.New("unknown setup channel")
	}
	out, err := patchDocument(expected, channel, value)
	if err != nil {
		return err
	}
	return writeUpdated(path, expected, out)
}

func patchDocument(data []byte, section string, value any) ([]byte, error) {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("configuration must be a mapping")
	}
	mapping := doc.Content[0]
	set := func(key string, node *yaml.Node) {
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == key {
				node.HeadComment = mapping.Content[i+1].HeadComment
				node.LineComment = mapping.Content[i+1].LineComment
				mapping.Content[i+1] = node
				return
			}
		}
		mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, node)
	}
	set("config_version", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(Version)})
	if section != "" {
		var node yaml.Node
		if err := node.Encode(value); err != nil {
			return nil, errors.New("cannot encode setup settings")
		}
		set(section, &node)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, errors.New("cannot encode configuration")
	}
	return out, nil
}

func writeUpdated(path string, expected, data []byte) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("configuration must be an existing regular file")
	}
	current, err := readConfig(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, expected) {
		return errors.New("configuration changed during setup; rerun setup to preserve your edits")
	}
	backup := path + ".backup-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := WriteNewBytes(backup, expected); err != nil {
		return err
	}
	var random [16]byte
	_, _ = rand.Read(random[:])
	temp := filepath.Join(filepath.Dir(path), ".config-"+hex.EncodeToString(random[:]))
	f, err := fileio.CreatePrivate(temp)
	if err != nil {
		return errors.New("cannot create private configuration output")
	}
	defer os.Remove(temp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot write configuration; backup was preserved")
	}
	current, err = readConfig(path)
	if err != nil || !bytes.Equal(current, expected) {
		return errors.New("configuration changed during setup; backup was preserved")
	}
	if err := fileio.ReplacePrivate(temp, path); err != nil {
		if errors.Is(err, fileio.ErrUnsafePermissions) {
			return errors.New("existing Windows configuration permissions are too broad or unsupported; restrict access to its owner, SYSTEM, Administrators and read-only LocalService using Windows Security settings or icacls before retrying (the installer establishes the supported service ACL); the original and private backup were preserved")
		}
		return errors.New("cannot replace configuration or preserve its access permissions; backup was preserved")
	}
	return nil
}

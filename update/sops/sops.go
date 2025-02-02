// Package sops provides an updater that uses the sops lib to update sops-encrypted files.
package sops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"go.mozilla.org/sops/v3"
	"go.mozilla.org/sops/v3/aes"
	"go.mozilla.org/sops/v3/cmd/sops/common"
	"go.mozilla.org/sops/v3/cmd/sops/formats"
	"go.mozilla.org/sops/v3/keyservice"

	"github.com/dailymotion-oss/octopilot/update/value"
)

// SopsUpdater is an updater that uses the sops lib to update sops-encrypted files.
type SopsUpdater struct {
	FilePath string
	Key      string
	Valuer   value.Valuer
}

// NewUpdater builds a new SOPS updater from the given parameters and valuer
func NewUpdater(params map[string]string, valuer value.Valuer) (*SopsUpdater, error) {
	updater := &SopsUpdater{}

	updater.FilePath = params["file"]
	if len(updater.FilePath) == 0 {
		return nil, errors.New("missing file parameter")
	}

	updater.Key = params["key"]
	if len(updater.Key) == 0 {
		return nil, errors.New("missing key parameter")
	}

	updater.Valuer = valuer

	return updater, nil
}

// Update updates the repository cloned at the given path, and returns true if changes have been made
func (u SopsUpdater) Update(ctx context.Context, repoPath string) (bool, error) {
	var (
		cipher = aes.NewCipher()
		svcs   = []keyservice.KeyServiceClient{keyservice.NewLocalClient()}
	)

	value, err := u.Valuer.Value(ctx, repoPath)
	if err != nil {
		return false, fmt.Errorf("failed to get value: %w", err)
	}

	filePaths, err := filepath.Glob(filepath.Join(repoPath, u.FilePath))
	if err != nil {
		return false, fmt.Errorf("failed to expand glob pattern %s: %w", u.FilePath, err)
	}

	var updated bool
	for _, filePath := range filePaths {
		relFilePath, err := filepath.Rel(repoPath, filePath)
		if err != nil {
			relFilePath = filePath
		}

		fileInfo, err := os.Stat(filePath)
		if err != nil {
			return false, fmt.Errorf("failed to access file %s: %w", relFilePath, err)
		}

		var (
			format = formats.FormatForPath(filePath)
			store  = common.StoreForFormat(format)
		)

		tree, err := common.LoadEncryptedFileWithBugFixes(common.GenericDecryptOpts{
			Cipher:      cipher,
			InputStore:  store,
			InputPath:   filePath,
			KeyServices: svcs,
		})
		if err != nil {
			return false, fmt.Errorf("failed to load encrypted file %s: %w", filePath, err)
		}

		dataKey, err := common.DecryptTree(common.DecryptTreeOpts{
			Cipher:      cipher,
			Tree:        tree,
			KeyServices: svcs,
		})
		if err != nil {
			return false, fmt.Errorf("failed to decrypt tree for %s: %w", filePath, err)
		}

		originalData, err := store.EmitPlainFile(tree.Branches)
		if err != nil {
			return false, fmt.Errorf("failed to emit original tree for %s: %w", filePath, err)
		}

		path := convertKeyToPath(u.Key)
		for i := range tree.Branches {
			newTree := tree.Branches[i].Set(path, value)
			// fix for https://github.com/mozilla/sops/issues/407
			// to be removed once https://github.com/mozilla/sops/pull/899 gets merged & released
			if previousTreeHasBeenErased(tree.Branches[i], newTree) {
				// if the path top-level element doesn't exist, it will return a new tree with only our path
				// the workaround is to add a single-level item first, and then the whole new branch
				rootEntry := []interface{}{
					path[0],
				}
				newTree = tree.Branches[i].Set(rootEntry, value)
				newTree = newTree.Set(path, value)
			}
			tree.Branches[i] = newTree
		}

		// check if we updated something or not, before re-encrypting...
		updatedData, err := store.EmitPlainFile(tree.Branches)
		if err != nil {
			return false, fmt.Errorf("failed to emit updated tree for %s: %w", filePath, err)
		}
		if string(updatedData) == string(originalData) {
			continue
		}

		err = common.EncryptTree(common.EncryptTreeOpts{
			DataKey: dataKey,
			Tree:    tree,
			Cipher:  cipher,
		})
		if err != nil {
			return false, fmt.Errorf("failed to encrypt tree for %s: %w", filePath, err)
		}

		encryptedFile, err := store.EmitEncryptedFile(*tree)
		if err != nil {
			return false, fmt.Errorf("failed to generate re-encrypted file %s: %w", filePath, err)
		}

		err = os.WriteFile(filePath, encryptedFile, fileInfo.Mode())
		if err != nil {
			return false, fmt.Errorf("failed to write re-encrypted data to file %s: %w", filePath, err)
		}

		updated = true
	}

	return updated, nil
}

// Message returns the default title and body that should be used in the commits / pull requests
func (u SopsUpdater) Message() (title, body string) {
	title = fmt.Sprintf("Update %s %s", u.FilePath, u.Key)
	body = fmt.Sprintf("Updating sops-encrypted file `%s` key `%s`", u.FilePath, u.Key)
	return title, body
}

// String returns a string representation of the updater
func (u SopsUpdater) String() string {
	return fmt.Sprintf("Sops[key=%s,file=%s]", u.Key, u.FilePath)
}

func convertKeyToPath(key string) []interface{} {
	path := make([]interface{}, 0)
	for _, entry := range strings.Split(key, ".") {
		path = append(path, entry)
	}
	return path
}

func previousTreeHasBeenErased(previous, next sops.TreeBranch) bool {
	if len(next) != 1 {
		// when the previous tree is "erased", the new one will have a single entry
		return false
	}

	if len(previous) != 1 {
		// if the tree size has changed, the previous tree has been erased
		return true
	}

	if reflect.DeepEqual(previous[0].Key, next[0].Key) {
		// same size, same key -> it's a simple tree with 1 element which hasn't changed
		return false
	}

	// otherwise, it's a 1 element tree which has changed
	return true
}

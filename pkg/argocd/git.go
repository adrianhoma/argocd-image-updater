package argocd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/order"
	kyaml "sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/argoproj-labs/argocd-image-updater/ext/git"
	"github.com/argoproj-labs/argocd-image-updater/registry-scanner/pkg/image"
	"github.com/argoproj-labs/argocd-image-updater/registry-scanner/pkg/log"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// TemplateCommitMessage renders a commit message template and returns it
// as a string, including image labels that can be used to add custom information to commit messages.
// If the template could not be rendered, returns a default
// message.
func TemplateCommitMessage(ctx context.Context, tpl *template.Template, appName string, changeList []ChangeEntry) string {
	log := log.LoggerFromContext(ctx)
	var cmBuf bytes.Buffer

	type commitMessageChange struct {
		Image  string
		OldTag string
		NewTag string
		Labels map[string]string
	}

	type commitMessageTemplate struct {
		AppName    string
		AppChanges []commitMessageChange
	}

	// We need to transform the change list into something more viable for the
	// writer of a template.
	changes := make([]commitMessageChange, 0)
	for _, c := range changeList {
		changes = append(changes, commitMessageChange{
			Image:  c.Image.ImageName,
			OldTag: c.OldTag.String(),
			NewTag: c.NewTag.String(),
			Labels: c.NewTag.Labels,
		})
	}

	tplData := commitMessageTemplate{
		AppName:    appName,
		AppChanges: changes,
	}
	err := tpl.Execute(&cmBuf, tplData)
	if err != nil {
		log.Errorf("could not execute template for Git commit message: %v", err)
		return "build: update of application " + appName
	}

	return cmBuf.String()
}

// TemplateBranchName parses a string to a template, and returns a
// branch name from that new template. If a branch name can not be
// rendered, it returns an empty value.
func TemplateBranchName(ctx context.Context, branchName, appNamespace, appName, targetKey string, changeList []ChangeEntry) string {
	log := log.LoggerFromContext(ctx)
	var cmBuf bytes.Buffer

	tpl, err1 := template.New("branchName").Parse(branchName)

	if err1 != nil {
		log.Errorf("could not create template for Git branch name: %v", err1)
		return ""
	}

	type imageChange struct {
		Name   string
		Alias  string
		OldTag string
		NewTag string
	}

	type branchNameTemplate struct {
		AppNamespace string
		AppName      string
		TargetKey    string
		Images       []imageChange
		SHA256       string
	}

	// Let's add a unique hash to the template
	hasher := sha256.New()

	// We need to transform the change list into something more viable for the
	// writer of a template.
	changes := make([]imageChange, 0)
	for _, c := range changeList {
		changes = append(changes, imageChange{c.Image.ImageName, c.Image.ImageAlias, c.OldTag.String(), c.NewTag.String()})
		id := fmt.Sprintf("%v-%v-%v,", c.Image.ImageName, c.OldTag.String(), c.NewTag.String())
		_, hasherErr := hasher.Write([]byte(id))
		log.Infof("writing to hasher %v", id)
		if hasherErr != nil {
			log.Errorf("could not write image string to hasher: %v", hasherErr)
			return ""
		}
	}

	tplData := branchNameTemplate{
		AppNamespace: appNamespace,
		AppName:      appName,
		TargetKey:    targetKey,
		Images:       changes,
		SHA256:       hex.EncodeToString(hasher.Sum(nil)),
	}

	err2 := tpl.Execute(&cmBuf, tplData)
	if err2 != nil {
		log.Errorf("could not execute template for Git branch name: %v", err2)
		return ""
	}

	toReturn := cmBuf.String()

	if len(toReturn) > 255 {
		trunc := toReturn[:255]
		log.Warnf("write-branch name %v exceeded 255 characters and was truncated to %v", toReturn, trunc)
		return trunc
	} else {
		return toReturn
	}
}

type changeWriter func(ctx context.Context, applicationImages *ApplicationImages, gitC git.Client) (err error, skip bool)

// getWriteBackBranch returns the branch to use for write-back operations.
// It first checks for a branch specified in annotations, then uses the
// targetRevision from the matching git source, falling back to getApplicationSource.
func getWriteBackBranch(ctx context.Context, app *v1alpha1.Application, wbc *WriteBackConfig) string {
	log := log.LoggerFromContext(ctx)
	if app == nil {
		return ""
	}
	// If git repository is specified, find matching source
	if wbc != nil && wbc.GitRepo != "" {
		gitRepo := wbc.GitRepo
		if app.Spec.HasMultipleSources() {
			for _, s := range app.Spec.Sources {
				if s.RepoURL == gitRepo {
					log.Debugf("Using target revision '%s' from matching source '%s'", s.TargetRevision, gitRepo)
					return s.TargetRevision
				}
			}
			log.Debugf("No matching source found for git repository %s, falling back to primary source", gitRepo)
		}
	}
	// Fall back to getApplicationSource's targetRevision
	// This maintains consistency with how other parts of the code select the source
	return getApplicationSource(ctx, app, wbc).TargetRevision
}

// commitChangesGit commits any changes required for updating one or more images
// after the UpdateApplication cycle has finished.
func commitChangesGit(ctx context.Context, applicationImages *ApplicationImages, changeList []ChangeEntry, write changeWriter) error {
	logCtx := log.LoggerFromContext(ctx)

	app := applicationImages.Application
	wbc := applicationImages.WriteBackConfig
	creds, err := wbc.GetCreds(&app)
	if err != nil {
		return fmt.Errorf("could not get creds for repo '%s': %v", wbc.GitRepo, err)
	}
	var gitC git.Client
	if wbc.GitClient == nil {
		tempRoot, err := os.MkdirTemp(os.TempDir(), fmt.Sprintf("git-%s", app.Name))
		if err != nil {
			return err
		}
		defer func() {
			err := os.RemoveAll(tempRoot)
			if err != nil {
				logCtx.Errorf("could not remove temp dir: %v", err)
			}
		}()
		gitC, err = git.NewClientExt(wbc.GitRepo, tempRoot, creds, false, false, "")
		if err != nil {
			return err
		}
	} else {
		gitC = wbc.GitClient
	}
	err = gitC.Init(ctx)
	if err != nil {
		return err
	}

	// The branch to checkout is either a configured branch in the write-back
	// config, or taken from the application spec's targetRevision. If the
	// target revision is set to the special value HEAD, or is the empty
	// string, we'll try to resolve it to a branch name.
	var checkOutBranch string
	if wbc.GitBranch != "" {
		checkOutBranch = wbc.GitBranch
	} else {
		checkOutBranch = getWriteBackBranch(ctx, &app, wbc)
	}
	logCtx.Tracef("targetRevision for update is '%s'", checkOutBranch)
	if checkOutBranch == "" || checkOutBranch == "HEAD" {
		checkOutBranch, err = gitC.SymRefToBranch(ctx, checkOutBranch)
		logCtx.Infof("resolved remote default branch to '%s' and using that for operations", checkOutBranch)
		if err != nil {
			return err
		}
	}

	// The push branch is by default the same as the checkout branch, unless
	// specified after a : separator git-branch annotation, in which case a
	// new branch will be made following a template that can use the list of
	// changed images.
	pushBranch := checkOutBranch

	// Set custom pushBranch name for PR/MR mode
	if wbc.PRProvider > 0 {
		customTemplate := PRBranchTemplate
		logCtx.Tracef("setting git push branch for PR/MR mode using custom template '%s'", customTemplate)
		pushBranch = TemplateBranchName(ctx, customTemplate, app.Namespace, app.Name, wbc.WriteBackTargetKey(), changeList)
		if pushBranch == "" {
			return fmt.Errorf("git branch name could not be created from the template: %s", customTemplate)
		}
		wbc.PullRequest, err = buildPullRequest(ctx, wbc, app.Namespace, app.Name, checkOutBranch, pushBranch)
		if err != nil {
			return err
		}
	} else if wbc.GitWriteBranch != "" {
		// use GitWriteBranch for git mode without PR
		logCtx.Debugf("Using branch template: %s", wbc.GitWriteBranch)
		pushBranch = TemplateBranchName(ctx, wbc.GitWriteBranch, "", "", "", changeList)
		if pushBranch == "" {
			return fmt.Errorf("git branch name could not be created from the template: %s", wbc.GitWriteBranch)
		}
	}

	// If the pushBranch already exists in the remote origin, directly use it.
	// Otherwise, create the new pushBranch from checkoutBranch
	pushBranchCreated := false
	if checkOutBranch != pushBranch {
		fetchErr := gitC.ShallowFetch(ctx, pushBranch, 1)
		if fetchErr != nil {
			err = gitC.ShallowFetch(ctx, checkOutBranch, 1)
			if err != nil {
				return err
			}
			logCtx.Debugf("Creating branch '%s' and using that for push operations", pushBranch)
			err = gitC.Branch(ctx, checkOutBranch, pushBranch)
			if err != nil {
				return err
			}
			pushBranchCreated = true
		}
	} else {
		err = gitC.ShallowFetch(ctx, checkOutBranch, 1)
		if err != nil {
			return err
		}
	}

	err = gitC.Checkout(ctx, pushBranch, false)
	if err != nil {
		return err
	}

	if err, skip := write(ctx, applicationImages, gitC); err != nil {
		return err
	} else if skip {
		return nil
	}

	// In API commit mode, hand the prepared working tree over to the GitHub
	// API instead of committing and pushing locally. Only GitHub App
	// credentials produce GitHub-signed commits; any other credential type
	// falls back to the normal git command-line path.
	if wbc.GitCommitMethod == GitCommitMethodAPI {
		if tokenProvider, ok := githubAppCredsProvider(creds); ok {
			return commitChangesGithubAPI(ctx, wbc, gitC, tokenProvider, pushBranch, pushBranchCreated)
		}
		logCtx.Warnf("git-commit-method 'api' requires GitHub App credentials for repo '%s', falling back to git command-line commit", wbc.GitRepo)
	}

	commitOpts := &git.CommitOptions{}
	if wbc.GitCommitMessage != "" {
		cm, err := os.CreateTemp("", "image-updater-commit-msg")
		if err != nil {
			return fmt.Errorf("could not create temp file: %v", err)
		}
		logCtx.Debugf("Writing commit message to %s", cm.Name())
		err = os.WriteFile(cm.Name(), []byte(wbc.GitCommitMessage), 0600)
		if err != nil {
			_ = cm.Close()
			return fmt.Errorf("could not write commit message to %s: %v", cm.Name(), err)
		}
		commitOpts.CommitMessagePath = cm.Name()
		_ = cm.Close()
		defer os.Remove(cm.Name())
	}

	// Set username and e-mail address used to identify the commiter
	if wbc.GitCommitUser != "" && wbc.GitCommitEmail != "" {
		err = gitC.Config(ctx, wbc.GitCommitUser, wbc.GitCommitEmail)
		if err != nil {
			return err
		}
	}

	if wbc.GitCommitSigningKey != "" {
		commitOpts.SigningKey = wbc.GitCommitSigningKey
	}

	commitOpts.SigningMethod = wbc.GitCommitSigningMethod
	commitOpts.SignOff = wbc.GitCommitSignOff

	err = gitC.Commit(ctx, "", commitOpts)
	if err != nil {
		return err
	}
	err = gitC.Push(ctx, "origin", pushBranch, pushBranch != checkOutBranch)
	if err != nil {
		return err
	}

	return nil
}

func writeOverrides(ctx context.Context, applicationImages *ApplicationImages, gitC git.Client) (err error, skip bool) {
	logCtx := log.LoggerFromContext(ctx)
	wbc := applicationImages.WriteBackConfig
	targetExists := true
	targetFile := path.Join(gitC.Root(), wbc.Target)
	_, err = os.Stat(targetFile)
	if err != nil {
		if !os.IsNotExist(err) {
			return
		} else {
			targetExists = false
		}
	}

	// If the target file already exist in the repository, we will check whether
	// our generated new file is the same as the existing one, and if yes, we
	// don't proceed further for commit.
	var override []byte
	var originalData []byte
	if targetExists {
		originalData, err = os.ReadFile(targetFile)
		if err != nil {
			return err, false
		}
		override, err = marshalParamsOverride(ctx, applicationImages, originalData)
		if err != nil {
			return
		}
		if string(originalData) == string(override) {
			logCtx.Debugf("target parameter file and marshaled data are the same, skipping commit.")
			return nil, true
		}
	} else {
		override, err = marshalParamsOverride(ctx, applicationImages, nil)
		if err != nil {
			return
		}
	}

	dir := filepath.Dir(targetFile)
	err = os.MkdirAll(dir, 0700)
	if err != nil {
		return
	}

	err = os.WriteFile(targetFile, override, 0600)
	if err != nil {
		return
	}

	if !targetExists {
		err = gitC.Add(ctx, targetFile)
	}
	return
}

var _ changeWriter = writeOverrides

// writeKustomization writes any changes required for updating one or more images to a kustomization.yml
func writeKustomization(ctx context.Context, applicationImages *ApplicationImages, gitC git.Client) (err error, skip bool) {
	app := applicationImages.Application
	wbc := applicationImages.WriteBackConfig
	logCtx := log.LoggerFromContext(ctx)

	base := filepath.Join(gitC.Root(), wbc.KustomizeBase)

	logCtx.Infof("updating base %s", base)

	kustFile := findKustomization(base)
	if kustFile == "" {
		return fmt.Errorf("could not find kustomization in %s", base), false
	}
	source := getApplicationSource(ctx, &app, wbc)
	if source == nil {
		return fmt.Errorf("failed to find source for kustomization in %s", base), false
	}

	kustomize := source.Kustomize
	images := v1alpha1.KustomizeImages{}
	if kustomize != nil {
		images = kustomize.Images
	}

	filterFunc, err := imagesFilter(ctx, images)
	if err != nil {
		return err, false
	}

	return updateKustomizeFile(ctx, filterFunc, kustFile)
}

// updateKustomizeFile reads the kustomization file at path, applies the filter to it, and writes the result back
// to the file. This is the same behavior as kyaml.UpdateFile, but it preserves the original order of YAML fields,
// indentation of YAML sequences, blank lines, and a leading document-start marker to minimize git diffs.
func updateKustomizeFile(ctx context.Context, filter kyaml.Filter, path string) (error, bool) {
	log := log.LoggerFromContext(ctx)

	// Open the input file for read
	yRaw, err := os.ReadFile(path)
	if err != nil {
		return err, false
	}

	// kio's ByteReadWriter discards a leading "---" document-start marker
	// while parsing and never re-emits one for a single document. Detected
	// after the leading run of blank lines/comments, not just at byte 0, so a
	// marker preceded by a header comment is still found - that whole prefix
	// (comments, blank lines, and the marker itself) sits outside any
	// document node in kyaml's model and is dropped during parsing too, so
	// both are restored together, verbatim, from these captured original
	// bytes rather than from anything kyaml round-trips.
	yPrefixLen := leadingCommentPrefixLen(yRaw)
	hadDocStart := hasDocumentStartAt(yRaw, yPrefixLen)

	// Encode blank lines as marker comments so they survive the kyaml round-trip.
	// kyaml (go.yaml.in/yaml/v3) discards blank lines during parsing but preserves
	// head comments, so we convert blank lines to comments and restore them afterward.
	yEncoded := encodeBlankLines(yRaw)

	// kio's YAML parser hard-errors on any input containing a "%YAML"/"%TAG"
	// directive line, even though that content sits outside any document node
	// and would otherwise just be dropped. Strip the whole leading prefix
	// (comments, blank-line markers, and directives alike) before handing
	// input to kio, since it's restored verbatim from yRaw at write time
	// regardless of what kio does with it.
	yBody := yEncoded[leadingCommentPrefixLen(yEncoded):]

	// Read the yaml document from bytes (use encoded to keep comparison consistent)
	originalYSlice, err := kio.FromBytes(yBody)
	if err != nil {
		return err, false
	}

	// Check that we are dealing with a single document
	if len(originalYSlice) != 1 {
		return errors.New("target parameter file should contain a single YAML document"), false
	}
	originalY := originalYSlice[0]

	// Get the (parsed) original document
	originalData, err := originalY.String()
	if err != nil {
		return err, false
	}

	// Create a reader, preserving indentation of sequences
	var out bytes.Buffer
	rw := &kio.ByteReadWriter{
		Reader:            bytes.NewBuffer(yBody),
		Writer:            &out,
		PreserveSeqIndent: true,
	}

	// Read from input buffer
	newYSlice, err := rw.Read()
	if err != nil {
		return err, false
	}
	// We can safely assume we have a single document from the previous check
	newY := newYSlice[0]

	// Update the yaml
	if err := newY.PipeE(filter); err != nil {
		return err, false
	}

	// Preserve the original order of fields
	if err := order.SyncOrder(originalY, newY); err != nil {
		return err, false
	}

	// Write the yaml document to the output buffer
	if err = rw.Write([]*kyaml.RNode{newY}); err != nil {
		return err, false
	}

	// newY contains metadata used by kio to preserve sequence indentation,
	// hence we need to parse the output buffer instead
	newParsedY, err := kyaml.Parse(out.String())
	if err != nil {
		return err, false
	}
	newData, err := newParsedY.String()
	if err != nil {
		return err, false
	}

	// Compare the updated document with the original document
	if originalData == newData {
		log.Debugf("target parameter file and marshaled data are the same, skipping commit.")
		return nil, true
	}

	// Write to file the changes, restoring blank lines from the marker comments,
	// the leading comment/blank-line prefix, and the document-start marker, if
	// the original had one. Content before "---" sits outside any document node
	// in kyaml's model - it's dropped entirely during parsing, not merely
	// un-preserved, so it never resurfaces in out.Bytes() no matter what
	// encodeBlankLines does. It has to be spliced back in verbatim from the
	// original bytes rather than searched for in the output. This applies
	// whether or not the original had a document-start marker - a leading
	// comment on a markerless file is just as much a part of the dropped
	// prefix as one before "---".
	outBytes := decodeBlankLines(out.Bytes())
	restored := make([]byte, 0, yPrefixLen+4+len(outBytes))
	restored = append(restored, yRaw[:yPrefixLen]...)
	if hadDocStart && !bytes.HasPrefix(outBytes, []byte("---")) {
		restored = append(restored, "---\n"...)
	}
	outBytes = append(restored, outBytes...)
	if err := os.WriteFile(path, outBytes, 0600); err != nil {
		return err, false
	}

	return nil, false
}

func imagesFilter(ctx context.Context, images v1alpha1.KustomizeImages) (kyaml.Filter, error) {
	var overrides []types.Image
	for _, img := range images {
		imgSet := parseImageOverride(img)
		if imgSet.Name == "" {
			return nil, fmt.Errorf("invalid kustomize image override %q: no image name", string(img))
		}
		overrides = append(overrides, imgSet)
	}

	return kyaml.FilterFunc(func(object *kyaml.RNode) (*kyaml.RNode, error) {
		if images, err := object.Pipe(kyaml.Lookup("images")); err != nil {
			return nil, err
		} else if images != nil {
			normalizeNullSequence(images)
		}
		seq, err := object.Pipe(kyaml.LookupCreate(kyaml.SequenceNode, "images"))
		if err != nil {
			return nil, err
		}
		// An override may only merge into entries that were already in the
		// file, never into entries appended by an earlier override of the
		// same batch - otherwise the result would depend on override order.
		original, err := mappingElements(seq)
		if err != nil {
			return nil, err
		}
		originalSet := make(map[*kyaml.Node]bool, len(original))
		for _, elem := range original {
			originalSet[elem.YNode()] = true
		}
		for _, imgSet := range overrides {
			if err := applyImageOverride(ctx, seq, imgSet, originalSet); err != nil {
				return nil, err
			}
		}
		return object, nil
	}), nil
}

// applyImageOverride merges one image override into the images sequence. An
// entry is addressed by its name (kustomize's own matching), or - for an
// override without an alias - by a newName that produces the tracked image,
// so a rename the file already carries is updated in place instead of
// appended as a duplicate. Without a match, the override is appended as a
// new entry.
func applyImageOverride(ctx context.Context, seq *kyaml.RNode, imgSet types.Image, original map[*kyaml.Node]bool) error {
	logCtx := log.LoggerFromContext(ctx)
	elements, err := mappingElements(seq)
	if err != nil {
		return err
	}

	var matches []*kyaml.RNode
	for _, elem := range elements {
		if elementField(elem, "name") == imgSet.Name {
			matches = append(matches, elem)
		}
	}
	if len(matches) == 0 && imgSet.NewName == "" {
		// An override with an alias always addresses the entry named after
		// the alias; falling through to a final-image match instead would
		// retarget an entry belonging to a different source image.
		for _, elem := range elements {
			if !original[elem.YNode()] {
				continue
			}
			if newName := elementField(elem, "newName"); newName != "" && sameImageRef(newName, imgSet.Name) {
				logCtx.Debugf("updating kustomize entry %q in place: its newName %q produces image %q", elementField(elem, "name"), newName, imgSet.Name)
				matches = append(matches, elem)
			}
		}
	}

	if len(matches) == 0 {
		return appendImageElement(seq, imgSet)
	}
	for _, match := range matches {
		if err := mergeImageOverride(ctx, match, imgSet); err != nil {
			return err
		}
	}
	return nil
}

// mergeImageOverride updates a matched images entry in place. Only newTag and
// digest are written - an existing newName is never overwritten, since it may
// deliberately differ from the tracked image (e.g. a pull-through cache
// mirror). newName is only added when the entry has no newName field at all
// and it would not be redundant with name.
func mergeImageOverride(ctx context.Context, match *kyaml.RNode, imgSet types.Image) error {
	if imgSet.NewName != "" {
		if f := match.Field("newName"); f == nil {
			if elementField(match, "name") != imgSet.NewName {
				if err := setElementField(match, "newName", imgSet.NewName); err != nil {
					return err
				}
			}
		} else if existing := kyaml.GetValue(f.Value); !sameImageRef(existing, imgSet.NewName) {
			log.LoggerFromContext(ctx).Warnf("not overwriting newName %q with %q for kustomize image %q", existing, imgSet.NewName, imgSet.Name)
		}
	}
	// the override holds the complete desired tag/digest state, so a field it
	// lacks is stale and must be removed
	for _, fv := range []struct{ name, value string }{{"newTag", imgSet.NewTag}, {"digest", imgSet.Digest}} {
		if fv.value == "" {
			if err := match.PipeE(kyaml.Clear(fv.name)); err != nil {
				return err
			}
			continue
		}
		if err := setElementField(match, fv.name, fv.value); err != nil {
			return err
		}
	}
	return nil
}

// appendImageElement appends the override as a new images entry, built field
// by field so each scalar carries the style its own value requires.
func appendImageElement(seq *kyaml.RNode, imgSet types.Image) error {
	elem := kyaml.NewMapRNode(nil)
	for _, fv := range []struct{ name, value string }{
		{"name", imgSet.Name},
		{"newName", imgSet.NewName},
		{"newTag", imgSet.NewTag},
		{"digest", imgSet.Digest},
	} {
		if fv.value == "" {
			continue
		}
		if err := setElementField(elem, fv.name, fv.value); err != nil {
			return err
		}
	}
	seq.YNode().Content = append(seq.YNode().Content, elem.YNode())
	return nil
}

// setElementField writes a scalar field, replacing the whole value node so
// the style is derived from the new value, not inherited from the old one -
// kyaml's FieldSetter keeps an existing plain style in place, which would
// emit YAML 1.1 booleans like "on" unquoted and change their type on the
// next parse.
func setElementField(elem *kyaml.RNode, field, value string) error {
	node := kyaml.NewStringRNode(value)
	if kyaml.IsYaml1_1NonString(node.YNode()) {
		node.YNode().Style = kyaml.DoubleQuotedStyle
	}
	if f := elem.Field(field); f != nil {
		f.Value.SetYNode(node.YNode())
		return nil
	}
	return elem.PipeE(kyaml.FieldSetter{Name: field, Value: node, OverrideStyle: true})
}

// mappingElements returns the sequence's mapping elements, skipping elements
// of any other kind - the same elements kyaml's ElementMatcher considers.
func mappingElements(seq *kyaml.RNode) ([]*kyaml.RNode, error) {
	elements, err := seq.Elements()
	if err != nil {
		return nil, err
	}
	var mappings []*kyaml.RNode
	for _, elem := range elements {
		if elem.YNode().Kind == kyaml.MappingNode {
			mappings = append(mappings, elem)
		}
	}
	return mappings, nil
}

// normalizeNullSequence converts a null-valued node (a bare "images:" key)
// into an empty sequence, so appended entries land in the document - content
// appended under a null scalar is dropped at serialization time.
func normalizeNullSequence(node *kyaml.RNode) {
	y := node.YNode()
	if y.Kind == kyaml.ScalarNode && (y.Tag == kyaml.NodeTagNull || y.Tag == kyaml.NodeTagEmpty) && len(y.Content) == 0 {
		y.Kind = kyaml.SequenceNode
		y.Tag = kyaml.NodeTagSeq
		y.Value = ""
		y.Style = 0
	}
}

// sameImageRef reports whether two image references name the same image,
// tolerating the equivalent Docker Hub spellings (a docker.io/ or
// index.docker.io/ prefix, and the library/ namespace of official images).
func sameImageRef(a, b string) bool {
	return normalizeImageRef(a) == normalizeImageRef(b)
}

func normalizeImageRef(ref string) string {
	for _, prefix := range []string{"docker.io/", "index.docker.io/"} {
		if strings.HasPrefix(ref, prefix) {
			ref = strings.TrimPrefix(ref, prefix)
			break
		}
	}
	// only Docker Hub references (no registry host containing a dot or port)
	// carry an implicit library/ namespace
	if host, _, found := strings.Cut(ref, "/"); found && !strings.Contains(host, ".") && !strings.Contains(host, ":") {
		ref = strings.TrimPrefix(ref, "library/")
	}
	return ref
}

// elementField returns the scalar value of a mapping field, or "" when absent.
func elementField(node *kyaml.RNode, field string) string {
	f := node.Field(field)
	if f == nil {
		return ""
	}
	return kyaml.GetValue(f.Value)
}

const blankLineMarker = "# __preserve_blank_line__"

// encodeBlankLines replaces blank lines with a marker comment so they survive
// the kyaml round-trip. kyaml discards blank lines but preserves head comments.
func encodeBlankLines(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	end := len(lines)
	if end > 0 && lines[end-1] == "" {
		end-- // don't encode the trailing empty string from the final \n
	}
	for i := 0; i < end; i++ {
		if strings.TrimSpace(lines[i]) == "" {
			lines[i] = blankLineMarker
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// decodeBlankLines converts blank line markers back to actual blank lines.
func decodeBlankLines(data []byte) []byte {
	return bytes.ReplaceAll(data, []byte(blankLineMarker+"\n"), []byte("\n"))
}

// leadingCommentPrefixLen returns the byte length of data's leading run of
// blank lines, "#"-comment lines, and "%YAML"/"%TAG" directive lines - all of
// which sit outside any document node in kyaml's model and are dropped
// entirely during parsing, along with a "---" marker they precede. Used so
// that whole prefix can be located, captured, and spliced back in verbatim
// rather than searched for in kyaml's output.
func leadingCommentPrefixLen(data []byte) int {
	offset := 0
	for {
		line, rest, found := bytesCutNewline(data[offset:])
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) != 0 && trimmed[0] != '#' && trimmed[0] != '%' {
			return offset
		}
		if !found {
			return len(data)
		}
		// rest is a suffix of the original data slice (bytesCutNewline was
		// given data[offset:], a suffix of data), so this is the byte index
		// right after the newline just consumed.
		offset = len(data) - len(rest)
	}
}

// bytesCutNewline splits data at its first '\n', mirroring bytes.Cut for a
// single-byte separator, and reports whether a newline was actually found
// (data with no trailing newline is still one "line" for this purpose).
func bytesCutNewline(data []byte) (line, rest []byte, found bool) {
	if before, after, ok := bytes.Cut(data, []byte{'\n'}); ok {
		return before, after, true
	}
	return data, nil, false
}

// hasDocumentStartAt reports whether data's line starting at byte offset
// prefixLen is a YAML document-start marker ("---"), optionally followed by
// whitespace and a trailing comment (e.g. "--- # header").
func hasDocumentStartAt(data []byte, prefixLen int) bool {
	line, _, _ := bytesCutNewline(data[prefixLen:])
	// A document-start marker is only valid at column 0, so trim only
	// trailing whitespace here - an indented "---" is ordinary scalar
	// content, not a marker.
	line = bytes.TrimRight(line, " \t\r\n")
	if len(line) < 3 || string(line[:3]) != "---" {
		return false
	}
	return len(line) == 3 || line[3] == ' ' || line[3] == '\t'
}

func findKustomization(base string) string {
	for _, f := range konfig.RecognizedKustomizationFileNames() {
		kustFile := path.Join(base, f)
		if stat, err := os.Stat(kustFile); err == nil && !stat.IsDir() {
			return kustFile
		}
	}
	return ""
}

func parseImageOverride(str v1alpha1.KustomizeImage) types.Image {
	// TODO is this a valid use? format could diverge
	img := image.NewFromIdentifier(string(str))
	tagName := ""
	tagDigest := ""
	if img.ImageTag != nil {
		tagName = img.ImageTag.TagName
		tagDigest = img.ImageTag.TagDigest
	}
	if img.RegistryURL != "" {
		// NewFromIdentifier strips off the registry
		img.ImageName = img.RegistryURL + "/" + img.ImageName
	}
	if img.ImageAlias == "" {
		img.ImageAlias = img.ImageName
		img.ImageName = "" // inside baseball (see return): name isn't changing, just tag, so don't write newName
	}
	return types.Image{
		Name:    img.ImageAlias,
		NewName: img.ImageName,
		NewTag:  tagName,
		Digest:  tagDigest,
	}
}

var _ changeWriter = writeKustomization

package argocd

import (
	"context"
	"os"
	"testing"
	"text/template"
	"time"

	"github.com/argoproj-labs/argocd-image-updater/ext/git"
	gitmock "github.com/argoproj-labs/argocd-image-updater/ext/git/mocks"
	"github.com/argoproj-labs/argocd-image-updater/pkg/common"
	"github.com/argoproj-labs/argocd-image-updater/registry-scanner/pkg/image"
	"github.com/argoproj-labs/argocd-image-updater/registry-scanner/pkg/tag"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/kustomize/api/types"
	kyaml "sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func Test_TemplateCommitMessage(t *testing.T) {
	t.Run("Template default commit message", func(t *testing.T) {
		exp := `build: automatic update of foobar

updates image foo/bar tag '1.0' to '1.1'
updates image bar/baz tag '2.0' to '2.1'
`
		tpl := template.Must(template.New("sometemplate").Parse(common.DefaultGitCommitMessage))
		cl := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("foo/bar"),
				OldTag: tag.NewImageTag("1.0", time.Now(), ""),
				NewTag: tag.NewImageTag("1.1", time.Now(), ""),
			},
			{
				Image:  image.NewFromIdentifier("bar/baz"),
				OldTag: tag.NewImageTag("2.0", time.Now(), ""),
				NewTag: tag.NewImageTag("2.1", time.Now(), ""),
			},
		}
		r := TemplateCommitMessage(context.Background(), tpl, "foobar", cl)
		assert.NotEmpty(t, r)
		assert.Equal(t, exp, r)
	})

	t.Run("Template commit message with labels", func(t *testing.T) {
		tplStr := `build: automatic update of {{ .AppName }}

{{ range .AppChanges -}}
updates image {{ .Image }} tag '{{ .OldTag }}' to '{{ .NewTag }}'
{{ if index .Labels "org.opencontainers.image.revision" -}}
Upstream Commit: {{ index .Labels "org.opencontainers.image.source" }}/commit/{{ index .Labels "org.opencontainers.image.revision" }}
{{ end -}}
{{ end -}}`
		exp := `build: automatic update of foobar

updates image foo/bar tag '1.0' to '1.1'
Upstream Commit: https://github.com/org/repo/commit/abc123
`
		tpl := template.Must(template.New("labelstemplate").Parse(tplStr))
		cl := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("foo/bar"),
				OldTag: tag.NewImageTag("1.0", time.Now(), ""),
				NewTag: tag.NewImageTagWithLabels("1.1", time.Now(), "", map[string]string{
					"org.opencontainers.image.source":   "https://github.com/org/repo",
					"org.opencontainers.image.revision": "abc123",
				}),
			},
		}
		r := TemplateCommitMessage(context.Background(), tpl, "foobar", cl)
		assert.NotEmpty(t, r)
		assert.Equal(t, exp, r)
	})

	t.Run("Template commit message with empty labels", func(t *testing.T) {
		tplStr := `build: automatic update of {{ .AppName }}

{{ range .AppChanges -}}
updates image {{ .Image }} tag '{{ .OldTag }}' to '{{ .NewTag }}'
{{ if index .Labels "org.opencontainers.image.revision" -}}
Upstream Commit: {{ index .Labels "org.opencontainers.image.source" }}/commit/{{ index .Labels "org.opencontainers.image.revision" }}
{{ end -}}
{{ end -}}`
		exp := `build: automatic update of foobar

updates image foo/bar tag '1.0' to '1.1'
`
		tpl := template.Must(template.New("emptylabels").Parse(tplStr))
		cl := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("foo/bar"),
				OldTag: tag.NewImageTag("1.0", time.Now(), ""),
				NewTag: tag.NewImageTag("1.1", time.Now(), ""),
			},
		}
		r := TemplateCommitMessage(context.Background(), tpl, "foobar", cl)
		assert.NotEmpty(t, r)
		assert.Equal(t, exp, r)
	})
}

func Test_TemplateBranchName(t *testing.T) {
	t.Run("Template branch name with image name", func(t *testing.T) {
		exp := `image-updater-foo/bar-1.1-bar/baz-2.1`
		tpl := "image-updater{{range .Images}}-{{.Name}}-{{.NewTag}}{{end}}"
		cl := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("foo/bar"),
				OldTag: tag.NewImageTag("1.0", time.Now(), ""),
				NewTag: tag.NewImageTag("1.1", time.Now(), ""),
			},
			{
				Image:  image.NewFromIdentifier("bar/baz"),
				OldTag: tag.NewImageTag("2.0", time.Now(), ""),
				NewTag: tag.NewImageTag("2.1", time.Now(), ""),
			},
		}
		r := TemplateBranchName(context.Background(), tpl, "", "", "", cl)
		assert.NotEmpty(t, r)
		assert.Equal(t, exp, r)
	})
	t.Run("Template branch name with alias", func(t *testing.T) {
		exp := `image-updater-bar-1.1`
		tpl := "image-updater{{range .Images}}-{{.Alias}}-{{.NewTag}}{{end}}"
		cl := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("bar=0001.dkr.ecr.us-east-1.amazonaws.com/bar"),
				OldTag: tag.NewImageTag("1.0", time.Now(), ""),
				NewTag: tag.NewImageTag("1.1", time.Now(), ""),
			},
		}
		r := TemplateBranchName(context.Background(), tpl, "", "", "", cl)
		assert.NotEmpty(t, r)
		assert.Equal(t, exp, r)
	})
	t.Run("Template branch name with hash", func(t *testing.T) {
		// Expected value generated from https://emn178.github.io/online-tools/sha256.html
		exp := `image-updater-0fcc2782543e4bb067c174c21bf44eb947f3e55c0d62c403e359c1c209cbd041`
		tpl := "image-updater-{{.SHA256}}"
		cl := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("foo/bar"),
				OldTag: tag.NewImageTag("1.0", time.Now(), ""),
				NewTag: tag.NewImageTag("1.1", time.Now(), ""),
			},
		}
		r := TemplateBranchName(context.Background(), tpl, "", "", "", cl)
		assert.NotEmpty(t, r)
		assert.Equal(t, exp, r)
	})
	t.Run("Template branch over 255 chars", func(t *testing.T) {
		tpl := "image-updater-lorem-ipsum-dolor-sit-amet-consectetur-" +
			"adipiscing-elit-phasellus-imperdiet-vitae-elit-quis-pulvinar-" +
			"suspendisse-pulvinar-lacus-vel-semper-congue-enim-purus-posuere-" +
			"orci-ut-vulputate-mi-ipsum-quis-ipsum-quisque-elit-arcu-lobortis-" +
			"in-blandit-vel-pharetra-vel-urna-aliquam-euismod-elit-vel-mi"
		exp := tpl[:255]
		cl := []ChangeEntry{}
		r := TemplateBranchName(context.Background(), tpl, "", "", "", cl)
		assert.NotEmpty(t, r)
		assert.Equal(t, exp, r)
		assert.Len(t, r, 255)
	})
	t.Run("AppNamespace and AppName available at top level", func(t *testing.T) {
		tpl := "image-updater-{{.AppNamespace}}-{{.AppName}}"
		cl := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("nginx"),
				OldTag: tag.NewImageTag("1.0", time.Now(), ""),
				NewTag: tag.NewImageTag("1.1", time.Now(), ""),
			},
		}
		r := TemplateBranchName(context.Background(), tpl, "my-namespace", "my-app", "", cl)
		assert.Equal(t, "image-updater-my-namespace-my-app", r)
	})
	t.Run("AppNamespace and AppName are stable regardless of image changes", func(t *testing.T) {
		tpl := "image-updater-{{.AppNamespace}}-{{.AppName}}"
		clV1 := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("nginx"),
				OldTag: tag.NewImageTag("1.0", time.Now(), ""),
				NewTag: tag.NewImageTag("1.1", time.Now(), ""),
			},
		}
		clV2 := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("nginx"),
				OldTag: tag.NewImageTag("1.1", time.Now(), ""),
				NewTag: tag.NewImageTag("1.2", time.Now(), ""),
			},
		}
		r1 := TemplateBranchName(context.Background(), tpl, "my-namespace", "my-app", "", clV1)
		r2 := TemplateBranchName(context.Background(), tpl, "my-namespace", "my-app", "", clV2)
		assert.Equal(t, "image-updater-my-namespace-my-app", r1)
		assert.Equal(t, r1, r2, "branch name should be stable across different image updates")
	})
	t.Run("AppNamespace and AppName empty when not provided", func(t *testing.T) {
		tpl := "image-updater-{{.AppNamespace}}-{{.AppName}}"
		r := TemplateBranchName(context.Background(), tpl, "", "", "", []ChangeEntry{})
		assert.Equal(t, "image-updater--", r)
	})
	t.Run("AppName usable alongside image variables", func(t *testing.T) {
		tpl := "{{.AppName}}{{range .Images}}-{{.Name}}-{{.NewTag}}{{end}}"
		cl := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("nginx"),
				OldTag: tag.NewImageTag("1.0", time.Now(), ""),
				NewTag: tag.NewImageTag("1.5", time.Now(), ""),
			},
		}
		r := TemplateBranchName(context.Background(), tpl, "prod", "my-app", "", cl)
		assert.Equal(t, "my-app-nginx-1.5", r)
	})
	t.Run("TargetKey in template for PR dedup", func(t *testing.T) {
		tpl := "image-updater-{{.TargetKey}}-{{.SHA256}}"
		cl := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("nginx"),
				OldTag: tag.NewImageTag("1.0", time.Now(), ""),
				NewTag: tag.NewImageTag("1.1", time.Now(), ""),
			},
		}
		r := TemplateBranchName(context.Background(), tpl, "ns", "app-a", "abc12345", cl)
		assert.Contains(t, r, "abc12345", "branch should contain the target key hash")
		assert.NotContains(t, r, "app-a", "branch should not contain app name")
	})
	t.Run("Same target key and changes produce same branch for different apps", func(t *testing.T) {
		tpl := "image-updater-{{.TargetKey}}-{{.SHA256}}"
		cl := []ChangeEntry{
			{
				Image:  image.NewFromIdentifier("nginx"),
				OldTag: tag.NewImageTag("1.0", time.Now(), ""),
				NewTag: tag.NewImageTag("1.1", time.Now(), ""),
			},
		}
		r1 := TemplateBranchName(context.Background(), tpl, "ns", "app-a", "abc12345", cl)
		r2 := TemplateBranchName(context.Background(), tpl, "ns", "app-b", "abc12345", cl)
		assert.Equal(t, r1, r2, "different apps with same target key and changes should produce the same branch")
	})
}

func Test_parseImageOverride(t *testing.T) {
	cases := []struct {
		name     string
		override v1alpha1.KustomizeImage
		expected types.Image
	}{
		{"tag update", "ghcr.io:1234/foo/foo:123", types.Image{
			Name:   "ghcr.io:1234/foo/foo",
			NewTag: "123",
		}},
		{"image update", "ghcr.io:1234/foo/foo=ghcr.io:1234/bar", types.Image{
			Name:    "ghcr.io:1234/foo/foo",
			NewName: "ghcr.io:1234/bar",
		}},
		{"update everything", "ghcr.io:1234/foo/foo=1234.foo.com:9876/bar:123", types.Image{
			Name:    "ghcr.io:1234/foo/foo",
			NewName: "1234.foo.com:9876/bar",
			NewTag:  "123",
		}},
		{"change registry and tag", "ghcr.io:1234/foo/foo=1234.dkr.ecr.us-east-1.amazonaws.com/bar:123", types.Image{
			Name:    "ghcr.io:1234/foo/foo",
			NewName: "1234.dkr.ecr.us-east-1.amazonaws.com/bar",
			NewTag:  "123",
		}},
		{"change only registry", "0001.dkr.ecr.us-east-1.amazonaws.com/bar=1234.dkr.ecr.us-east-1.amazonaws.com/bar", types.Image{
			Name:    "0001.dkr.ecr.us-east-1.amazonaws.com/bar",
			NewName: "1234.dkr.ecr.us-east-1.amazonaws.com/bar",
		}},
		{"change image and set digest", "foo=acme/app@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", types.Image{
			Name:    "foo",
			NewName: "acme/app",
			Digest:  "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		}},
		{"set digest", "acme/app@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", types.Image{
			Name:   "acme/app",
			Digest: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, parseImageOverride(tt.override))
		})
	}

}

func Test_imagesFilter(t *testing.T) {
	for _, tt := range []struct {
		name     string
		images   v1alpha1.KustomizeImages
		expected string
	}{
		{name: "simple", images: v1alpha1.KustomizeImages{"foo"}, expected: `
images:
- name: foo
`},
		{name: "tagged", images: v1alpha1.KustomizeImages{"foo:bar"}, expected: `
images:
- name: foo
  newTag: bar
`},
		{name: "rename", images: v1alpha1.KustomizeImages{"baz=foo:bar"}, expected: `
images:
- name: baz
  newName: foo
  newTag: bar
`},
		{name: "digest", images: v1alpha1.KustomizeImages{"baz=foo@sha12345"}, expected: `
images:
- name: baz
  newName: foo
  digest: sha12345
`},
		{name: "digest simple", images: v1alpha1.KustomizeImages{"foo@sha12345"}, expected: `
images:
- name: foo
  digest: sha12345
`},
		{name: "all", images: v1alpha1.KustomizeImages{
			"foo",
			"foo=bar", // merges with above
			"baz@sha12345",
			"bar:123",     // appends: no pre-existing entry produces "bar"
			"foo=bar:123", // merges into the foo entry by alias
		}, expected: `
images:
- name: foo
  newName: bar
  newTag: "123"
- name: baz
  digest: sha12345
- name: bar
  newTag: "123"
`},
		// the same overrides produce the same set of entries in any order
		{name: "order independence", images: v1alpha1.KustomizeImages{
			"foo=bar:456",
			"bar:123",
		}, expected: `
images:
- name: foo
  newName: bar
  newTag: "456"
- name: bar
  newTag: "123"
`},
		{name: "order independence reversed", images: v1alpha1.KustomizeImages{
			"bar:123",
			"foo=bar:456",
		}, expected: `
images:
- name: bar
  newTag: "123"
- name: foo
  newName: bar
  newTag: "456"
`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			filter, err := imagesFilter(context.Background(), tt.images)
			assert.NoError(t, err)

			node := kyaml.NewRNode(&kyaml.Node{Kind: kyaml.DocumentNode, Content: []*kyaml.Node{
				kyaml.NewMapRNode(nil).YNode(),
			}})
			node, err = filter.Filter(node)
			assert.NoError(t, err)
			assert.YAMLEq(t, tt.expected, node.MustString())
		})
	}

	t.Run("rejects an override without an image name", func(t *testing.T) {
		_, err := imagesFilter(context.Background(), v1alpha1.KustomizeImages{""})
		assert.Error(t, err)
	})

	t.Run("filter application does not alias nodes across documents", func(t *testing.T) {
		filter, err := imagesFilter(context.Background(), v1alpha1.KustomizeImages{"foo:v1"})
		assert.NoError(t, err)

		first := kyaml.MustParse("resources: []\n")
		second := kyaml.MustParse("resources: []\n")
		_, err = filter.Filter(first)
		assert.NoError(t, err)
		_, err = filter.Filter(second)
		assert.NoError(t, err)

		// mutating the entry appended to one document must not leak into the other
		assert.NoError(t, second.PipeE(kyaml.Lookup("images", "[name=foo]"), kyaml.SetField("newTag", kyaml.NewStringRNode("v2"))))
		firstTag, err := first.Pipe(kyaml.Lookup("images", "[name=foo]", "newTag"))
		assert.NoError(t, err)
		assert.Equal(t, "v1", kyaml.GetValue(firstTag))
	})
}

func Test_updateKustomizeFile(t *testing.T) {
	makeTmpKustomization := func(t *testing.T, content []byte) string {
		f, err := os.CreateTemp("", "kustomization-*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Write(content)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		t.Cleanup(func() {
			os.Remove(f.Name())
		})
		return f.Name()
	}

	filter, err := imagesFilter(context.Background(), v1alpha1.KustomizeImages{"foo@sha23456"})
	if err != nil {
		t.Fatal(err)
	}

	mustFilter := func(images v1alpha1.KustomizeImages) kyaml.Filter {
		f, err := imagesFilter(context.Background(), images)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	tests := []struct {
		name        string
		content     string
		wantContent string
		filter      kyaml.Filter
		wantErr     bool
	}{
		{
			name: "sorted",
			content: `images:
- digest: sha12345
  name: foo
`,
			wantContent: `images:
- digest: sha23456
  name: foo
`,
			filter: filter,
		},
		{
			name: "not-sorted",
			content: `images:
- name: foo
  digest: sha12345
`,
			wantContent: `images:
- name: foo
  digest: sha23456
`,
			filter: filter,
		},
		{
			name: "indented",
			content: `images:
  - name: foo
    digest: sha12345
`,
			wantContent: `images:
  - name: foo
    digest: sha23456
`,
			filter: filter,
		},
		{
			name: "no-change",
			content: `images:
- name: foo
  digest: sha23456
`,
			wantContent: "",
			filter:      filter,
		},
		{
			name: "invalid-path",
			content: `images:
- name: foo
  digest: sha12345
`,
			wantContent: "",
			filter:      filter,
			wantErr:     true,
		},
		{
			name: "blank-lines-between-sections",
			content: `images:
- name: foo
  digest: sha12345

resources:
- some-resource.yaml
`,
			wantContent: `images:
- name: foo
  digest: sha23456

resources:
- some-resource.yaml
`,
			filter: filter,
		},
		{
			name: "blank-lines-before-images",
			content: `apiVersion: kustomize.config.k8s.io/v1beta1

images:
- name: foo
  digest: sha12345
`,
			wantContent: `apiVersion: kustomize.config.k8s.io/v1beta1

images:
- name: foo
  digest: sha23456
`,
			filter: filter,
		},
		{
			name: "preserves leading document-start marker",
			content: `---
images:
- name: foo
  digest: sha12345
`,
			wantContent: `---
images:
- name: foo
  digest: sha23456
`,
			filter: filter,
		},
		{
			name: "does not add a document-start marker when absent",
			content: `images:
- name: foo
  digest: sha12345
`,
			wantContent: `images:
- name: foo
  digest: sha23456
`,
			filter: filter,
		},
		{
			name: "preserves marker after a leading comment",
			content: `# leading comment
---
images:
- name: foo
  digest: sha12345
`,
			wantContent: `# leading comment
---
images:
- name: foo
  digest: sha23456
`,
			filter: filter,
		},
		{
			name: "preserves a leading comment when no marker is present",
			content: `# leading comment
images:
- name: foo
  digest: sha12345
`,
			wantContent: `# leading comment
images:
- name: foo
  digest: sha23456
`,
			filter: filter,
		},
		{
			name: "preserves a leading blank line when no marker is present",
			content: `
images:
- name: foo
  digest: sha12345
`,
			wantContent: `
images:
- name: foo
  digest: sha23456
`,
			filter: filter,
		},
		{
			name: "preserves marker after a leading blank line",
			content: `
---
images:
- name: foo
  digest: sha12345
`,
			wantContent: `
---
images:
- name: foo
  digest: sha23456
`,
			filter: filter,
		},
		{
			// Reproduces issue #312: an update addressed by alias must not overwrite an
			// existing newName that differs from the tracked image (e.g. a
			// pull-through cache mirror), and must only bump the tag.
			name: "preserves newName on alias update",
			content: `images:
- name: foo
  newName: mirror.example.com/registry/foo
  newTag: v1.0.0
`,
			wantContent: `images:
- name: foo
  newName: mirror.example.com/registry/foo
  newTag: v1.1.0
`,
			filter: mustFilter(v1alpha1.KustomizeImages{"foo=registry.example.com/foo:v1.1.0"}),
		},
		{
			// Reproduces issue #312: an override without an alias must find the entry whose
			// newName produces the tracked image instead of appending a
			// duplicate entry.
			name: "matches entry by newName instead of appending duplicate",
			content: `images:
- name: foo
  newName: registry.example.com/foo
  newTag: v1.0.0
`,
			wantContent: `images:
- name: foo
  newName: registry.example.com/foo
  newTag: v1.1.0
`,
			filter: mustFilter(v1alpha1.KustomizeImages{"registry.example.com/foo:v1.1.0"}),
		},
		{
			// A name match with an existing newName keeps the rename even
			// when the override carries no alias.
			name: "preserves newName on plain name match",
			content: `images:
- name: registry.example.com/foo
  newName: mirror.example.com/registry/foo
  newTag: v1.0.0
`,
			wantContent: `images:
- name: registry.example.com/foo
  newName: mirror.example.com/registry/foo
  newTag: v1.1.0
`,
			filter: mustFilter(v1alpha1.KustomizeImages{"registry.example.com/foo:v1.1.0"}),
		},
		{
			name: "appends entry when nothing matches",
			content: `images:
- name: foo
  newTag: v1.0.0
`,
			wantContent: `images:
- name: foo
  newTag: v1.0.0
- name: bar
  newTag: v2.0.0
`,
			filter: mustFilter(v1alpha1.KustomizeImages{"bar:v2.0.0"}),
		},
		{
			name: "switching from tag to digest clears newTag",
			content: `images:
- name: foo
  newName: registry.example.com/foo
  newTag: v1.0.0
`,
			wantContent: `images:
- name: foo
  newName: registry.example.com/foo
  digest: sha23456
`,
			filter: mustFilter(v1alpha1.KustomizeImages{"foo=registry.example.com/foo@sha23456"}),
		},
		{
			// An aliased override addresses only the entry named after its
			// alias; it must not hijack an entry for a different source
			// image whose name merely equals the override's final image.
			name: "aliased override does not hijack a final-image entry",
			content: `images:
- name: bar
  newTag: v1
`,
			wantContent: `images:
- name: bar
  newTag: v1
- name: foo
  newName: bar
  newTag: v2
`,
			filter: mustFilter(v1alpha1.KustomizeImages{"foo=bar:v2"}),
		},
		{
			name: "updates every entry sharing the same name",
			content: `images:
- name: foo
  newTag: a
- name: foo
  newTag: a
`,
			wantContent: `images:
- name: foo
  newTag: v2
- name: foo
  newTag: v2
`,
			filter: mustFilter(v1alpha1.KustomizeImages{"foo:v2"}),
		},
		{
			// YAML 1.1 booleans must be quoted when replacing a plain-style
			// value, or the next parse changes their type and kustomize
			// fails to unmarshal the file.
			name: "quotes a YAML 1.1 boolean tag",
			content: `images:
- name: foo
  newTag: v1.0.0
`,
			wantContent: `images:
- name: foo
  newTag: "on"
`,
			filter: mustFilter(v1alpha1.KustomizeImages{"foo:on"}),
		},
		{
			name: "populates a null-valued images key",
			content: `kind: Kustomization
images:
`,
			wantContent: `kind: Kustomization
images:
- name: foo
  digest: sha23456
`,
			filter: filter,
		},
		{
			// A newName explicitly set to null is a present field and must
			// not be overwritten, same as any other existing newName.
			name: "preserves an explicitly null newName",
			content: `images:
- name: foo
  newName: null
  newTag: v1
`,
			wantContent: `images:
- name: foo
  newName: null
  newTag: "2"
`,
			filter: mustFilter(v1alpha1.KustomizeImages{"foo=bar:2"}),
		},
		{
			// Docker Hub images spell the same reference with or without the
			// docker.io/ prefix; both must match the entry's newName.
			name: "matches newName across docker.io spellings",
			content: `images:
- name: app
  newName: docker.io/myorg/app
  newTag: v1
`,
			wantContent: `images:
- name: app
  newName: docker.io/myorg/app
  newTag: v3
`,
			filter: mustFilter(v1alpha1.KustomizeImages{"myorg/app:v3"}),
		},
		{
			name: "preserves marker after a leading YAML directive",
			content: `%YAML 1.2
---
images:
- name: foo
  digest: sha12345
`,
			wantContent: `%YAML 1.2
---
images:
- name: foo
  digest: sha23456
`,
			filter: filter,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			if tt.wantErr {
				path = "/invalid-path"
			} else {
				path = makeTmpKustomization(t, []byte(tt.content))
			}

			err, skip := updateKustomizeFile(context.Background(), tt.filter, path)
			if tt.wantErr {
				assert.Error(t, err)
				assert.False(t, skip)
			} else if tt.name == "no-change" {
				assert.Nil(t, err)
				assert.True(t, skip)
			} else {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				assert.Equal(t, tt.wantContent, string(got))
				assert.False(t, skip)
			}
		})
	}
}

func Test_getApplicationSource(t *testing.T) {
	t.Run("multi-source without git repo annotation", func(t *testing.T) {
		app := &v1alpha1.Application{
			ObjectMeta: v1.ObjectMeta{
				Name: "test-app",
			},
			Spec: v1alpha1.ApplicationSpec{
				Sources: v1alpha1.ApplicationSources{
					{
						RepoURL:        "https://charts.bitnami.com/bitnami",
						TargetRevision: "18.2.3",
						Chart:          "nginx",
						Helm:           &v1alpha1.ApplicationSourceHelm{},
					},
					{
						RepoURL:        "https://github.com/chengfang/image-updater-examples.git",
						TargetRevision: "main",
					},
				},
			},
		}

		source := getApplicationSource(context.Background(), app, nil)
		assert.Equal(t, "18.2.3", source.TargetRevision)
		assert.Equal(t, "https://charts.bitnami.com/bitnami", source.RepoURL)
	})

	t.Run("single source application", func(t *testing.T) {
		app := &v1alpha1.Application{
			ObjectMeta: v1.ObjectMeta{
				Name: "test-app",
			},
			Spec: v1alpha1.ApplicationSpec{
				Source: &v1alpha1.ApplicationSource{
					RepoURL:        "https://github.com/example/repo.git",
					TargetRevision: "main",
				},
			},
		}

		source := getApplicationSource(context.Background(), app, nil)
		assert.Equal(t, "main", source.TargetRevision)
		assert.Equal(t, "https://github.com/example/repo.git", source.RepoURL)
	})
}

func Test_getWriteBackBranch(t *testing.T) {
	t.Run("nil application", func(t *testing.T) {
		branch := getWriteBackBranch(context.Background(), nil, nil)
		assert.Equal(t, "", branch)
	})

	t.Run("matching git-repository annotation", func(t *testing.T) {
		app := &v1alpha1.Application{
			ObjectMeta: v1.ObjectMeta{
				Name: "test-app",
			},
			Spec: v1alpha1.ApplicationSpec{
				Sources: v1alpha1.ApplicationSources{
					{
						RepoURL:        "https://charts.bitnami.com/bitnami",
						TargetRevision: "18.2.3",
						Chart:          "nginx",
					},
					{
						RepoURL:        "https://github.com/chengfang/image-updater-examples.git",
						TargetRevision: "main",
					},
				},
			},
		}
		wbc := &WriteBackConfig{GitRepo: "https://github.com/chengfang/image-updater-examples.git"}

		branch := getWriteBackBranch(context.Background(), app, wbc)
		assert.Equal(t, "main", branch)
	})

	t.Run("fallback to primary source when no match", func(t *testing.T) {
		app := &v1alpha1.Application{
			ObjectMeta: v1.ObjectMeta{
				Name: "test-app",
			},
			Spec: v1alpha1.ApplicationSpec{
				Sources: v1alpha1.ApplicationSources{
					{
						RepoURL:        "https://charts.bitnami.com/bitnami",
						TargetRevision: "18.2.3",
						Chart:          "nginx",
						Helm:           &v1alpha1.ApplicationSourceHelm{},
					},
					{
						RepoURL:        "https://github.com/chengfang/image-updater-examples.git",
						TargetRevision: "main",
					},
				},
			},
		}

		branch := getWriteBackBranch(context.Background(), app, nil)
		assert.Equal(t, "18.2.3", branch)
	})

	t.Run("git-repository annotation with non-matching URL", func(t *testing.T) {
		app := &v1alpha1.Application{
			ObjectMeta: v1.ObjectMeta{
				Name: "test-app",
			},
			Spec: v1alpha1.ApplicationSpec{
				Sources: v1alpha1.ApplicationSources{
					{
						RepoURL:        "https://charts.bitnami.com/bitnami",
						TargetRevision: "18.2.3",
						Chart:          "nginx",
						Helm:           &v1alpha1.ApplicationSourceHelm{},
					},
				},
			},
		}
		wbc := &WriteBackConfig{GitRepo: "https://github.com/different/repo.git"}
		branch := getWriteBackBranch(context.Background(), app, wbc)
		assert.Equal(t, "18.2.3", branch)
	})
}

// Test_commitChangesGit_APIMethodFallsBackWithoutAppCreds verifies that
// git-commit-method=api with non-GitHub-App credentials logs a warning and
// falls back to the git CLI commit+push path.
func Test_commitChangesGit_APIMethodFallsBackWithoutAppCreds(t *testing.T) {
	// Note: this repo's generated ext/git/mocks.Client forwards only the
	// non-context arguments to mock.Called (see e.g. ShallowFetch/Checkout/
	// Commit/Push in ext/git/mocks/Client.go), so expectations below omit a
	// leading mock.Anything for ctx to match the actual Called(...) arity.
	gitMock := &gitmock.Client{}
	gitMock.On("Init").Return(nil)
	gitMock.On("ShallowFetch", "main").Return(nil)
	gitMock.On("Checkout", "main", false).Return(nil)
	gitMock.On("Commit", "", mock.Anything).Return(nil)
	gitMock.On("Push", "origin", "main", false).Return(nil)

	wbc := &WriteBackConfig{
		Method:          WriteBackGit,
		GitClient:       gitMock,
		GitBranch:       "main",
		GitCommitMethod: GitCommitMethodAPI,
		GitRepo:         "https://github.com/example/repo.git",
		GetCreds: func(app *v1alpha1.Application) (git.Creds, error) {
			return git.NopCreds{}, nil
		},
	}
	appImages := &ApplicationImages{
		Application:     v1alpha1.Application{ObjectMeta: v1.ObjectMeta{Name: "testapp"}},
		WriteBackConfig: wbc,
	}
	noopWriter := func(ctx context.Context, ai *ApplicationImages, gitC git.Client) (error, bool) {
		return nil, false
	}

	err := commitChangesGit(context.Background(), appImages, nil, noopWriter)
	require.NoError(t, err)
	gitMock.AssertCalled(t, "Commit", "", mock.Anything)
	gitMock.AssertCalled(t, "Push", "origin", "main", false)
	// The API commit path must not have been taken.
	gitMock.AssertNotCalled(t, "WorkingTreeChanges")
}

func Test_hasDocumentStartAt(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "marker at column 0", data: "---\nfoo: bar\n", want: true},
		{name: "marker at column 0 with trailing comment", data: "--- # header\nfoo: bar\n", want: true},
		{name: "indented marker is not a document start", data: "   ---\nfoo: bar\n", want: false},
		{name: "tab-indented marker is not a document start", data: "\t---\nfoo: bar\n", want: false},
		{name: "no marker", data: "foo: bar\n", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasDocumentStartAt([]byte(tt.data), 0))
		})
	}
}

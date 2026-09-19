package process

import (
	"reflect"
	"testing"
)

// The memory brake evicts these paths from the file cache after a kill: the
// weights, the projector and the draft model, in both flag spellings.
func TestProcess_ModelFilesFromArgs(t *testing.T) {
	args := []string{"/opt/llama-server", "--port", "9001", "-m", "/m/cq27-00001-of-00002.gguf",
		"--mmproj=/m/mmproj.gguf", "-md", "/m/draft.gguf", "-c", "262144", "--model", "/m/other.gguf", "-mm"}
	want := []string{"/m/cq27-00001-of-00002.gguf", "/m/mmproj.gguf", "/m/draft.gguf", "/m/other.gguf"}
	if got := ModelFiles(args); !reflect.DeepEqual(got, want) {
		t.Fatalf("ModelFiles = %q, want %q", got, want)
	}
	if got := ModelFiles([]string{"llama-server", "-hf", "org/repo:Q4_K_M"}); got != nil {
		t.Fatalf("an -hf model has no path on the command line, got %q", got)
	}
}

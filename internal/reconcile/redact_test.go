/*
Copyright 2026 The Aenix Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package reconcile

import "testing"

func TestRedact(t *testing.T) {
	for name, tc := range map[string]struct {
		text    string
		secrets []string
		want    string
	}{
		"replaces every occurrence": {"a s3 b s3", []string{"s3"}, "a *** b ***"},
		"replaces every secret":     {"x1 y2", []string{"x1", "y2"}, "*** ***"},
		"leaves clean text alone":   {"nothing here", []string{"s3"}, "nothing here"},
		// The short secret first would leave "key-" of the long one in the text.
		"longer secret wins over a contained one": {"key-1234", []string{"1234", "key-1234"}, "***"},
		// An empty secret would otherwise insert the mask between every character.
		"skips empty secrets": {"abc", []string{""}, "abc"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := redact(tc.text, tc.secrets); got != tc.want {
				t.Errorf("redact(%q, %q) = %q, want %q", tc.text, tc.secrets, got, tc.want)
			}
		})
	}
}

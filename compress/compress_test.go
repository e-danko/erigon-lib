/*
   Copyright 2021 Erigon contributors

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

package compress

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"testing"
)

func TestCompressEmptyDict(t *testing.T) {
	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "compressed")
	c, err := NewCompressor(context.Background(), t.Name(), file, tmpDir, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err = c.AddWord([]byte("word")); err != nil {
		t.Fatal(err)
	}
	if err = c.Compress(); err != nil {
		t.Fatal(err)
	}
	var d *Decompressor
	if d, err = NewDecompressor(file); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	g := d.MakeGetter()
	if !g.HasNext() {
		t.Fatalf("expected a word")
	}
	word, _ := g.Next(nil)
	if string(word) != "word" {
		t.Fatalf("expeced word, got (hex) %x", word)
	}
	if g.HasNext() {
		t.Fatalf("not expecting anything else")
	}
}

func TestCompressDict1(t *testing.T) {
	tmpDir := t.TempDir()
	file := path.Join(tmpDir, "compressed")
	t.Name()
	c, err := NewCompressor(context.Background(), t.Name(), file, tmpDir, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 100; i++ {
		if err = c.AddWord([]byte(fmt.Sprintf("longlongword %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err = c.Compress(); err != nil {
		t.Fatal(err)
	}
	var d *Decompressor
	if d, err = NewDecompressor(file); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	g := d.MakeGetter()
	i := 0
	for g.HasNext() {
		word, _ := g.Next(nil)
		expected := fmt.Sprintf("longlongword %d", i)
		if string(word) != expected {
			t.Errorf("expected %s, got (hex) %s", expected, word)
		}
		i++
	}
}

package task

import "time"

const Version = 1

const DefaultTimeout = 2 * time.Minute

type File struct {
	Version int
	Name    string
	Desc    string
	Vars    []Var
	Items   []Entry
}

type Var struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Entry struct {
	Item  *Item
	Block *Block
}

type Block struct {
	Desc  string
	Items []Entry
}

type Item struct {
	Name      string
	Desc      string
	Check     string
	Cmd       string
	Cmds      []string
	When      string
	Platforms []string
	Timeout   time.Duration

	File *FileSpec
	Dir  *DirSpec
}

type FileSpec struct {
	Path    string
	Mode    string
	Owner   string
	Group   string
	Content string
}

type DirSpec struct {
	Path  string
	Mode  string
	Owner string
	Group string
}

func (i *Item) builtin() bool { return i.File != nil || i.Dir != nil }

func (i *Item) scripts() []string {
	if len(i.Cmds) > 0 {
		return i.Cmds
	}
	if i.Cmd != "" {
		return []string{i.Cmd}
	}
	return nil
}

func (i *Item) timeout() time.Duration {
	if i.Timeout > 0 {
		return i.Timeout
	}
	return DefaultTimeout
}

func walkItems(items []Entry, fn func(*Item)) {
	for _, e := range items {
		switch {
		case e.Block != nil:
			walkItems(e.Block.Items, fn)
		case e.Item != nil:
			fn(e.Item)
		}
	}
}

package storemd

import "errors"

var NotFoundError = errors.New("NOT_FOUND")

type KeyValuePair struct {
	Key   string
	Value string
}

type ListArgs struct {
	Prefix     string
	StartAfter string // full key, including prefix
	Limit      int    // 0 means no limit
}

type Store interface {
	Get(key string) (value string, err error)
	Set(key, value string) (err error)
	Delete(key string) (err error)
	List(args ListArgs) (result []KeyValuePair, err error)
}

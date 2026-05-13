package main

import "time"

// timeNow is a tiny indirection so tests can stub time.Now without us
// reaching into stdlib. Lake CLI is the only caller today.
var timeNow = time.Now

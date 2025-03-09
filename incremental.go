// Copyright 2024 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package blueprint

import (
	"text/scanner"
)

type BuildActionCacheKey struct {
	Id        string
	InputHash uint64
}

type CachedProvider struct {
	Id    *providerKey
	Value *any
}

type BuildActionCachedData struct {
	Providers        []CachedProvider
	Pos              *scanner.Position
	OrderOnlyStrings []string
}

type BuildActionCache = map[BuildActionCacheKey]*BuildActionCachedData

type OrderOnlyStringsCache map[string][]string

type BuildActionCacheInput struct {
	PropertiesHash uint64
	ProvidersHash  [][]uint64
}

type Incremental interface {
	IncrementalSupported() bool
}

type IncrementalModule struct{}

func (m *IncrementalModule) IncrementalSupported() bool {
	return true
}

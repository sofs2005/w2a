// toolschema.go 把工具参数 schema 修成各模型都收的形状。
//
// 背景：上游对 tools[].function.parameters 做严格 JSON Schema 校验，不同模型各拒
// 一部分形状——DeepSeek 几乎全拒，Kimi/GPT/Gemini/GLM 各拒一部分（400 code=11133
// 「Invalid request parameters」，社区在真实账号上逐项实测，2026-10-08/14）。
// 这些拒绝**与账号无关**：换号拿到的是同一个 400，所以只能发送前把形状修好。
//
// 归一无损：每个修复都保持「不窄化也不放宽」调用能携带的内容，只有空 enum（本
// 来就没有值能匹配）被删掉。规则与社区实测表一一对应：
//   - 根节点无 type / type 为空 / 根是 union（anyOf/oneOf/allOf）→ 合并成 object；
//   - 无 type 的 oneOf，每个分支都是 object（含本地 $ref 链）→ 补 type:object；
//   - tuple 的 items（数组）→ 一个 anyOf（单项直接取该项）；
//   - pattern 的 \0 → \u0000（语义相同）；精确的代理对守卫正则上游会拒，删掉；
//   - enum 无 type → 取值的类型（或该类型 + null）；多类型 enum → 按类型拆 anyOf；
//     空 enum → 删除；
//   - 布尔 schema（true/false 出现在 schema 位）→ {} / {"not":{}}；
//   - required 不是数组 → 删；type 不在 JSON Schema 词表里 → 删；"String" → "string"。
package upstream

import (
	"encoding/json"

	"strings"
)

// schemaTypes 是 JSON Schema 认的 type 值。
var schemaTypes = map[string]bool{
	"string": true, "number": true, "integer": true, "boolean": true,
	"object": true, "array": true, "null": true,
}

// schemaSubOne / schemaSubList / schemaSubMap：值是 schema / schema 列表 /
// 名字到 schema 的映射的关键字。additionalProperties 与 unevaluatedProperties
// 允许布尔值，不参与布尔 schema 转换。
var (
	schemaSubOne  = []string{"items", "additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else", "propertyNames", "additionalProperties", "unevaluatedProperties"}
	schemaSubList = []string{"anyOf", "oneOf", "allOf", "prefixItems"}
	schemaSubMap  = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
	schemaBoolOK  = map[string]bool{"additionalProperties": true, "unevaluatedProperties": true}
)

// fitToolShapes 就地修 tools/functions 的 parameters。返回是否有改动。
func fitToolShapes(obj map[string]any) bool {
	changed := false
	for _, key := range []string{"tools", "functions"} {
		list, ok := obj[key].([]any)
		if !ok {
			continue
		}
		for _, raw := range list {
			t, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			var params any
			if fn, ok := t["function"].(map[string]any); ok {
				params = fn["parameters"]
			} else {
				params = t["parameters"]
			}
			if params == nil {
				continue
			}
			ps, ok := params.(map[string]any)
			if !ok {
				if _, isBool := params.(bool); isBool {
					fixParams(t, map[string]any{"type": "object", "properties": map[string]any{}})
					changed = true
				}
				continue
			}
			// 各步独立执行（不用 || 短路，否则后面的修复不会跑）。
			if fitSchema(ps, 0) {
				changed = true
			}
			if fitShape(ps, 0) {
				changed = true
			}
			if typeUnions(ps, ps, 0) {
				changed = true
			}
			if objectRoot(ps, ps, 0) {
				changed = true
			}
		}
	}
	return changed
}

// fixParams 把非对象形态的 parameters 换成给定的对象（工具的 function 或裸条目）。
func fixParams(t map[string]any, ps map[string]any) {
	if fn, ok := t["function"].(map[string]any); ok {
		fn["parameters"] = ps
		return
	}
	t["parameters"] = ps
}

// fitSchema 修 pattern 与 tuple items（逐层）。
func fitSchema(node any, depth int) bool {
	if depth > 64 || node == nil {
		return false
	}
	m, ok := node.(map[string]any)
	if !ok {
		if list, ok := node.([]any); ok {
			changed := false
			for _, v := range list {
				if fitSchema(v, depth+1) {
					changed = true
				}
			}
			return changed
		}
		return false
	}
	changed := false
	if p, ok := m["pattern"].(string); ok {
		if np := fixNulEscape(p); np != p {
			m["pattern"] = np
			changed = true
		}
		if m["pattern"] == `^[^\uD800-\uDFFF]*$` {
			delete(m, "pattern")
			changed = true
		}
	}
	if items, ok := m["items"].([]any); ok {
		list := make([]any, 0, len(items))
		for _, v := range items {
			if _, ok := v.(map[string]any); ok {
				list = append(list, v)
			}
		}
		switch len(list) {
		case 0:
			m["items"] = map[string]any{}
		case 1:
			m["items"] = list[0]
		default:
			m["items"] = map[string]any{"anyOf": list}
		}
		delete(m, "additionalItems")
		changed = true
	}
	for _, v := range m {
		if fitSchema(v, depth+1) {
			changed = true
		}
	}
	return changed
}

// fixNulEscape 把 `\0` 换成 `\u0000`（匹配同一个字符，但 DeepSeek 的校验器只认后者）。
// RE2 不支持否定环视，这里手写扫描：跳过 `\\`（转义的反斜杠）与八进制转义 `\01`。
func fixNulEscape(p string) string {
	if !strings.Contains(p, `\0`) {
		return p
	}
	var b strings.Builder
	b.Grow(len(p) + 4)
	for i := 0; i < len(p); {
		if p[i] == '\\' && i+1 < len(p) {
			if p[i+1] == '\\' {
				b.WriteString(`\\`)
				i += 2
				continue
			}
			if p[i+1] == '0' && (i+2 >= len(p) || p[i+2] < '0' || p[i+2] > '9') {
				b.WriteString(`\u0000`)
				i += 2
				continue
			}
		}
		b.WriteByte(p[i])
		i++
	}
	return b.String()
}

// valueType 是枚举值对应的 JSON Schema 类型。
func valueType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if x == float64(int64(x)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return ""
}

// fitEnum 给 enum 补/拆 type，见文件头。
func fitEnum(s map[string]any) bool {
	raw, has := s["enum"]
	if !has {
		return false
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		delete(s, "enum")
		return true
	}
	// 已声明恰好一种非 null 类型 → 保持原样。
	listed := schemaTypeList(s["type"])
	nonNull := 0
	for _, t := range listed {
		if t != "null" {
			nonNull++
		}
	}
	if nonNull == 1 {
		return false
	}
	byType := map[string][]any{}
	order := []string{}
	for _, v := range list {
		t := valueType(v)
		if _, seen := byType[t]; !seen {
			order = append(order, t)
		}
		byType[t] = append(byType[t], v)
	}
	if ints, ok := byType["integer"]; ok {
		byType["number"] = append(byType["number"], ints...)
		delete(byType, "integer")
	}
	_, hasNull := byType["null"]
	delete(byType, "null")
	// 按原出现顺序收集非 null 类型（integer 已并入 number，去重）。
	var types []string
	typeSeen := map[string]bool{}
	for _, t := range order {
		if t == "integer" {
			t = "number"
		}
		if _, ok := byType[t]; !ok || typeSeen[t] {
			continue
		}
		typeSeen[t] = true
		types = append(types, t)
	}
	if len(types) <= 1 {
		var want any
		switch {
		case len(types) == 0:
			want = "null"
		case hasNull:
			want = []any{types[0], "null"}
		default:
			want = types[0]
		}
		if schemaTypeEqual(want, s["type"]) {
			return false
		}
		s["type"] = want
		return true
	}
	split := make([]any, 0, len(types)+1)
	for _, t := range types {
		split = append(split, map[string]any{"type": t, "enum": byType[t]})
	}
	if hasNull {
		split = append(split, map[string]any{"type": "null"})
	}
	delete(s, "enum")
	delete(s, "type")
	if _, ok := s["anyOf"].([]any); ok {
		// 已有 anyOf：把按类型拆开的分支并进 allOf，避免覆盖原有分支。
		s["allOf"] = append(schemaList(s["allOf"]), map[string]any{"anyOf": split})
	} else {
		s["anyOf"] = split
	}
	return true
}

// schemaTypeList 把 type 值归一成字符串列表。
func schemaTypeList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{strings.ToLower(t)}
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, strings.ToLower(s))
			}
		}
		return out
	}
	return nil
}

// schemaTypeEqual 比较两个 type 值（字符串或字符串数组）语义是否相同。
func schemaTypeEqual(a, b any) bool {
	as, bs := schemaTypeList(a), schemaTypeList(b)
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func schemaList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

// subSchema 把 schema 位上的布尔值换成对象：true → {}，false → {"not":{}}。
// 第二个返回值表示是否真的换了（避免对 map 做接口比较，那种比较会 panic）。
func subSchema(v any, key string) (any, bool) {
	b, ok := v.(bool)
	if !ok || schemaBoolOK[key] {
		return v, false
	}
	if b {
		return map[string]any{}, true
	}
	return map[string]any{"not": map[string]any{}}, true
}

// fitShape 修 type / required / enum，并递归到子 schema。
func fitShape(s map[string]any, depth int) bool {
	if depth > 64 {
		return false
	}
	changed := false
	if raw, ok := s["type"]; ok {
		list := schemaTypeList(raw)
		uniq := make([]string, 0, len(list))
		seen := map[string]bool{}
		for _, t := range list {
			if schemaTypes[t] && !seen[t] {
				seen[t] = true
				uniq = append(uniq, t)
			}
		}
		var want any
		switch len(uniq) {
		case 0:
			want = nil
		case 1:
			want = uniq[0]
		default:
			arr := make([]any, len(uniq))
			for i, t := range uniq {
				arr[i] = t
			}
			want = arr
		}
		if want == nil {
			delete(s, "type")
			changed = true
		} else if !schemaEqual(want, raw) {
			s["type"] = want
			changed = true
		}
	}
	if raw, ok := s["required"]; ok {
		list, ok := raw.([]any)
		if !ok {
			delete(s, "required")
			changed = true
		} else {
			kept := make([]any, 0, len(list))
			for _, v := range list {
				if str, ok := v.(string); ok {
					kept = append(kept, str)
				}
			}
			if len(kept) != len(list) {
				s["required"] = kept
				changed = true
			}
		}
	}
	if fitEnum(s) {
		changed = true
	}
	for _, k := range schemaSubOne {
		v, ok := s[k]
		if !ok {
			continue
		}
		if nv, ch := subSchema(v, k); ch {
			s[k] = nv
			v = nv
			changed = true
		}
		if m, ok := v.(map[string]any); ok && fitShape(m, depth+1) {
			changed = true
		}
	}
	for _, k := range schemaSubList {
		list, ok := s[k].([]any)
		if !ok {
			// oneOf/anyOf 出现在非数组位置：交给 typeUnions/上游原样报错。
			continue
		}
		for i, v := range list {
			nv, ch := subSchema(v, k)
			if ch {
				list[i] = nv
				changed = true
			}
			if m, ok := nv.(map[string]any); ok && fitShape(m, depth+1) {
				changed = true
			}
		}
	}
	for _, k := range schemaSubMap {
		m, ok := s[k].(map[string]any)
		if !ok {
			continue
		}
		for name, v := range m {
			nv, ch := subSchema(v, k)
			if ch {
				m[name] = nv
				changed = true
			}
			if sm, ok := nv.(map[string]any); ok && fitShape(sm, depth+1) {
				changed = true
			}
		}
	}
	return changed
}

// localRef 解析本地 $ref（#/$defs/x、#/definitions/x）。第二个返回值表示
// 「确实解析到了另一个 schema」；无 $ref/解析不到 → false（调用方保留原值）。
func localRef(root any, s map[string]any) (map[string]any, bool) {
	ref, ok := s["$ref"].(string)
	if !ok || !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	at := root
	for _, k := range strings.Split(ref[2:], "/") {
		k = strings.ReplaceAll(strings.ReplaceAll(k, "~1", "/"), "~0", "~")
		am, ok := at.(map[string]any)
		if !ok {
			return nil, false
		}
		at = am[k]
	}
	target, ok := at.(map[string]any)
	if !ok {
		return nil, false
	}
	return target, true
}

// objectBranch 判断分支是否（经本地 $ref 链）是 object 类型。
func objectBranch(root any, b any, seen []string) bool {
	m := asSchemaMap(b)
	if m == nil {
		return false
	}
	if t, ok := m["type"]; ok {
		return t == "object"
	}
	ref, ok := m["$ref"].(string)
	if !ok || len(seen) > 16 {
		return false
	}
	for _, s := range seen {
		if s == ref {
			return false // 环
		}
	}
	target, ok := localRef(root, m)
	if !ok {
		return false
	}
	return objectBranch(root, target, append(seen, ref))
}

// typeUnions 给「无 type、所有分支都是 object」的 oneOf 补 type:object。
func typeUnions(s any, root any, depth int) bool {
	if depth > 64 {
		return false
	}
	m, ok := s.(map[string]any)
	if !ok {
		return false
	}
	changed := false
	if _, hasType := m["type"]; !hasType {
		if oneOf, ok := m["oneOf"].([]any); ok && len(oneOf) > 0 {
			all := true
			for _, b := range oneOf {
				if !objectBranch(root, b, nil) {
					all = false
					break
				}
			}
			if all {
				m["type"] = "object"
				changed = true
			}
		}
	}
	for _, k := range schemaSubOne {
		if typeUnions(m[k], root, depth+1) {
			changed = true
		}
	}
	for _, k := range schemaSubList {
		for _, v := range schemaList(m[k]) {
			if typeUnions(v, root, depth+1) {
				changed = true
			}
		}
	}
	for _, k := range schemaSubMap {
		if mm, ok := m[k].(map[string]any); ok {
			for _, v := range mm {
				if typeUnions(v, root, depth+1) {
					changed = true
				}
			}
		}
	}
	return changed
}

// objectRoot 把工具的根参数修成 object：allOf 分支整体合并；anyOf/oneOf 的 object
// 分支合并 properties，共同的 required 保留。返回是否有改动。
func objectRoot(ps map[string]any, root any, depth int) bool {
	unions := make([]string, 0, 3)
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		if _, ok := ps[k].([]any); ok {
			unions = append(unions, k)
		}
	}
	if ps["type"] == "object" && len(unions) == 0 {
		return false
	}
	props, _ := ps["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	var required []any
	if list, ok := ps["required"].([]any); ok {
		required = append(required, list...)
	}
	if allOf, ok := ps["allOf"].([]any); ok {
		for _, b := range allOf {
			bm := asSchemaMap(b)
			if r, ok := localRef(root, bm); ok {
				bm = r
			}
			if bm == nil {
				continue
			}
			if depth < 8 && hasUnions(bm) {
				bm = cloneSchemaMap(bm)
				objectRoot(bm, root, depth+1)
			}
			for k, v := range mapOf(bm["properties"]) {
				if _, exists := props[k]; !exists {
					props[k] = v
				}
			}
			if list, ok := bm["required"].([]any); ok {
				required = append(required, list...)
			}
		}
	}
	// anyOf/oneOf 的 object 分支：properties 取并集（同名不同 schema → anyOf），
	// required 取交集（每个分支都要求的字段）。
	var branches []map[string]any
	for _, key := range []string{"anyOf", "oneOf"} {
		list, ok := ps[key].([]any)
		if !ok {
			continue
		}
		for _, b := range list {
			bm := asSchemaMap(b)
			if r, ok := localRef(root, bm); ok {
				bm = r
			}
			if bm == nil {
				continue
			}
			if bm["type"] != "object" && bm["properties"] == nil {
				continue
			}
			if depth < 8 && hasUnions(bm) {
				bm = cloneSchemaMap(bm)
				objectRoot(bm, root, depth+1)
			}
			branches = append(branches, bm)
		}
	}
	from := map[string][]any{}
	fromOrder := []string{}
	for _, b := range branches {
		for k, v := range mapOf(b["properties"]) {
			if _, exists := props[k]; exists {
				continue
			}
			if _, seen := from[k]; !seen {
				fromOrder = append(fromOrder, k)
			}
			from[k] = appendSchemaUnique(from[k], v)
		}
	}
	for _, k := range fromOrder {
		list := from[k]
		if len(list) == 1 {
			props[k] = list[0]
		} else {
			props[k] = map[string]any{"anyOf": list}
		}
	}
	if len(branches) > 0 {
		first := mapOf(branches[0]["properties"])
		for k := range first {
			all := true
			for _, b := range branches {
				if !containsString(mapOf(b["required"]), k) {
					all = false
					break
				}
			}
			if all {
				required = append(required, k)
			}
		}
	}
	for _, k := range unions {
		delete(ps, k)
	}
	ps["type"] = "object"
	ps["properties"] = props
	req := make([]any, 0, len(required))
	seen := map[string]bool{}
	for _, v := range required {
		s, ok := v.(string)
		if !ok || seen[s] {
			continue
		}
		if _, ok := props[s]; !ok {
			continue
		}
		seen[s] = true
		req = append(req, s)
	}
	if len(req) > 0 {
		ps["required"] = req
	} else {
		delete(ps, "required")
	}
	return true
}

func asSchemaMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func hasUnions(m map[string]any) bool {
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		if _, ok := m[k].([]any); ok {
			return true
		}
	}
	return false
}

func cloneSchemaMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func mapOf(v any) map[string]any {
	return asSchemaMap(v)
}

func appendSchemaUnique(list []any, v any) []any {
	for _, x := range list {
		if schemaEqual(x, v) {
			return list
		}
	}
	return append(list, v)
}

// schemaEqual 比较两个 schema 片段是否相同（仅用于 anyOf 去重；JSON 序列化后比较，
// map 键序由 encoding/json 归一）。
func schemaEqual(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ab) == string(bb)
}

func containsString(list any, want string) bool {
	arr, ok := list.([]any)
	if !ok {
		return false
	}
	for _, v := range arr {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}

package main

import (
	"fmt"
	"strconv"
)

const (
	subgraphInputNode  = -10
	subgraphOutputNode = -20
)

func workflowSubgraphs(workflow map[string]any) map[string]map[string]any {
	definitions, _ := workflow["definitions"].(map[string]any)
	graphs := make(map[string]map[string]any)
	for _, raw := range asSlice(definitions["subgraphs"]) {
		graph, _ := raw.(map[string]any)
		for _, field := range []string{"name", "id"} {
			if name, _ := graph[field].(string); name != "" {
				graphs[name] = graph
			}
		}
	}
	return graphs
}

type uiNodeSlot struct {
	node, slot int
}

type subgraphInstance struct {
	graph, host           map[string]any
	parent                *subgraphInstance
	hostID                int
	prefix                string
	inputNode, outputNode int
	nodes                 map[int]map[string]any
	links                 map[uiNodeSlot]uiNodeSlot
	children              map[int]*subgraphInstance
	widgetOverrides       map[int]map[string]any
}

func parseUIEdge(raw any) (id, origin, originSlot, target, targetSlot int, ok bool) {
	var fields []any
	switch link := raw.(type) {
	case []any:
		fields = link
	case map[string]any:
		fields = []any{link["id"], link["origin_id"], link["origin_slot"], link["target_id"], link["target_slot"]}
	}
	if len(fields) < 3 {
		return 0, 0, 0, 0, 0, false
	}
	var values [5]int
	for index, raw := range fields[:min(len(fields), len(values))] {
		value, valid := uiNodeID(raw)
		if !valid {
			return 0, 0, 0, 0, 0, false
		}
		values[index] = value
	}
	return values[0], values[1], values[2], values[3], values[4], true
}

func uiNodeID(raw any) (int, bool) {
	if value, ok := numericValue(raw); ok && value == float64(int(value)) {
		return int(value), true
	}
	if text, ok := raw.(string); ok {
		id, err := strconv.Atoi(text)
		return id, err == nil
	}
	return 0, false
}

func subgraphsToAPI(ui map[string]any, definitions map[string]map[string]any, order widgetOrderLookup) (map[string]any, error) {
	root := &subgraphInstance{graph: ui}
	remainingNodes := 100_000
	if err := root.expand(definitions, map[string]bool{}, &remainingNodes); err != nil {
		return nil, err
	}
	payload := make(map[string]any)
	root.writePayload(payload, order)
	return payload, nil
}

func (g *subgraphInstance) expand(definitions map[string]map[string]any, ancestors map[string]bool, remainingNodes *int) error {
	rawNodes, ok := g.graph["nodes"].([]any)
	if !ok || len(ancestors) > 32 {
		return fmt.Errorf("%w：子图 %s 缺少节点或嵌套过深", errUnsupportedWorkflowFeature, g.prefix)
	}
	*remainingNodes -= len(rawNodes)
	if *remainingNodes < 0 {
		return fmt.Errorf("%w：展开后的节点数过多", errUnsupportedWorkflowFeature)
	}
	g.nodes = make(map[int]map[string]any, len(rawNodes))
	g.links = make(map[uiNodeSlot]uiNodeSlot)
	g.children = make(map[int]*subgraphInstance)
	for _, raw := range asSlice(g.graph["links"]) {
		if _, origin, slot, target, targetSlot, ok := parseUIEdge(raw); ok {
			g.links[uiNodeSlot{target, targetSlot}] = uiNodeSlot{origin, slot}
		}
	}
	for _, raw := range rawNodes {
		node, _ := raw.(map[string]any)
		id, ok := uiNodeID(node["id"])
		if !ok {
			continue
		}
		g.nodes[id] = node
		kind, _ := node["type"].(string)
		if explicit, ok := node["subgraphId"].(string); ok {
			kind = explicit
		}
		definition := definitions[kind]
		mode, _ := uiNodeID(node["mode"])
		if definition == nil || mode == nodeModeNever || mode == nodeModeBypass {
			continue
		}
		key, _ := definition["id"].(string)
		if key == "" {
			key = kind
		}
		if ancestors[key] {
			return fmt.Errorf("%w：子图 %s 循环引用", errUnsupportedWorkflowFeature, key)
		}
		child := &subgraphInstance{
			graph: definition, host: node, parent: g, hostID: id, prefix: g.nodeKey(id) + ":",
			inputNode:  subgraphBoundaryID(definition, "inputNode", subgraphInputNode),
			outputNode: subgraphBoundaryID(definition, "outputNode", subgraphOutputNode),
		}
		ancestors[key] = true
		err := child.expand(definitions, ancestors, remainingNodes)
		delete(ancestors, key)
		if err != nil {
			return err
		}
		g.children[id] = child
	}
	if g.parent != nil {
		g.readLegacyWidgets()
	}
	return nil
}

func subgraphBoundaryID(graph map[string]any, field string, fallback int) int {
	boundary, _ := graph[field].(map[string]any)
	if id, ok := uiNodeID(boundary["id"]); ok {
		return id
	}
	return fallback
}

func (g *subgraphInstance) nodeKey(id int) string {
	return g.prefix + strconv.Itoa(id)
}

// resolve follows a link across nested graph boundaries. A boundary may also
// supply a literal from a promoted widget on this particular host instance.
func (g *subgraphInstance) resolve(source uiNodeSlot, depth int) (any, bool) {
	if depth >= maxLinkHops {
		return nil, false
	}
	if g.parent != nil && source.node == g.inputNode {
		hostSlot := correspondingPort(asSlice(g.graph["inputs"]), asSlice(g.host["inputs"]), source.slot)
		if parentSource, ok := g.parent.links[uiNodeSlot{g.hostID, hostSlot}]; ok {
			return g.parent.resolve(parentSource, depth+1)
		}
		return g.hostWidgetValue(source.slot, hostSlot)
	}
	node := g.nodes[source.node]
	if node == nil {
		return nil, false
	}
	if child := g.children[source.node]; child != nil {
		outputSlot := correspondingPort(asSlice(child.host["outputs"]), asSlice(child.graph["outputs"]), source.slot)
		if inner, ok := child.links[uiNodeSlot{child.outputNode, outputSlot}]; ok {
			return child.resolve(inner, depth+1)
		}
		return nil, false
	}
	if nodeSkipReason(node) != "" {
		if upstream, ok := g.links[uiNodeSlot{source.node, source.slot}]; ok {
			return g.resolve(upstream, depth+1)
		}
		return nil, false
	}
	return []any{g.nodeKey(source.node), source.slot}, true
}

// Modern exports can omit unconnected host ports. Match by name when both
// sides have port metadata and use the recorded slot index for legacy exports.
func correspondingPort(from, to []any, slot int) int {
	if slot < 0 || slot >= len(from) || len(to) == 0 {
		return slot
	}
	source, _ := from[slot].(map[string]any)
	name, _ := source["name"].(string)
	if name == "" {
		return slot
	}
	if slot < len(to) {
		candidate, _ := to[slot].(map[string]any)
		if candidate["name"] == name {
			return slot
		}
	}
	for index, raw := range to {
		port, _ := raw.(map[string]any)
		if port["name"] == name {
			return index
		}
	}
	return -1
}

func (g *subgraphInstance) hostWidgetValue(definitionSlot, hostSlot int) (any, bool) {
	hostInputs := asSlice(g.host["inputs"])
	definitionInputs := asSlice(g.graph["inputs"])
	var name string
	if definitionSlot >= 0 && definitionSlot < len(definitionInputs) {
		definition, _ := definitionInputs[definitionSlot].(map[string]any)
		name, _ = definition["name"].(string)
	}
	named, _ := g.host["widgets_values_named"].(map[string]any)
	if hostSlot >= 0 && hostSlot < len(hostInputs) {
		input, _ := hostInputs[hostSlot].(map[string]any)
		if hostName, _ := input["name"].(string); hostName != "" {
			name = hostName
		}
		widget, _ := input["widget"].(map[string]any)
		if widgetName, _ := widget["name"].(string); widgetName != "" {
			if value, ok := named[widgetName]; ok {
				return expandDateTemplate(value), true
			}
		}
	}
	if value, ok := named[name]; name != "" && ok {
		return expandDateTemplate(value), true
	}
	values := asSlice(g.host["widgets_values"])
	properties, _ := g.host["properties"].(map[string]any)
	for index, raw := range asSlice(properties["proxyWidgets"]) {
		pair := asSlice(raw)
		if len(pair) >= 2 && pair[1] == name && index < len(values) {
			if id, ok := uiNodeID(pair[0]); ok && id == -1 {
				return expandDateTemplate(values[index]), true
			}
		}
	}
	position := 0
	for index, raw := range hostInputs {
		input, _ := raw.(map[string]any)
		if input["widget"] == nil {
			continue
		}
		if index == hostSlot && position < len(values) {
			return expandDateTemplate(values[position]), true
		}
		position++
	}
	return nil, false
}

func (g *subgraphInstance) readLegacyWidgets() {
	properties, _ := g.host["properties"].(map[string]any)
	values := asSlice(g.host["widgets_values"])
	named, _ := g.host["widgets_values_named"].(map[string]any)
	for index, raw := range asSlice(properties["proxyWidgets"]) {
		pair := asSlice(raw)
		if len(pair) < 2 {
			continue
		}
		id, valid := uiNodeID(pair[0])
		name, _ := pair[1].(string)
		if !valid || g.nodes[id] == nil || name == "" {
			continue
		}
		value, ok := named[name]
		if !ok && index < len(values) {
			value, ok = values[index], true
		}
		if !ok {
			continue
		}
		if g.widgetOverrides == nil {
			g.widgetOverrides = make(map[int]map[string]any)
		}
		if g.widgetOverrides[id] == nil {
			g.widgetOverrides[id] = make(map[string]any)
		}
		g.widgetOverrides[id][name] = expandDateTemplate(value)
	}
}

func (g *subgraphInstance) writePayload(payload map[string]any, order widgetOrderLookup) {
	for id, node := range g.nodes {
		if child := g.children[id]; child != nil {
			child.writePayload(payload, order)
			continue
		}
		if nodeSkipReason(node) != "" {
			continue
		}
		classType, _ := node["type"].(string)
		inputs := make(map[string]any)
		for slot, raw := range asSlice(node["inputs"]) {
			input, _ := raw.(map[string]any)
			name, _ := input["name"].(string)
			if source, found := g.links[uiNodeSlot{id, slot}]; name != "" && found {
				if value, ok := g.resolve(source, 0); ok {
					inputs[name] = value
				}
			}
		}
		for name, value := range g.widgetOverrides[id] {
			if _, linked := inputs[name]; !linked && !frontendOnlyWidgetNames[name] {
				inputs[name] = value
			}
		}
		applyUIWidgetValues(inputs, node, classType, order)
		payload[g.nodeKey(id)] = map[string]any{"class_type": classType, "inputs": inputs}
	}
}

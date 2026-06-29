package main

import "fmt"

// modelItem is a row in the Models list.
type modelItem struct {
	name      string
	family    string
	params    string
	quant     string
	size      string
	isDefault bool
}

func (i modelItem) Title() string {
	if i.isDefault {
		return "★ " + i.name + "  (default)"
	}
	return i.name
}
func (i modelItem) Description() string {
	return fmt.Sprintf("%s · %s · %s · %s",
		dashIf(i.family), dashIf(i.params), dashIf(i.quant), dashIf(i.size))
}
func (i modelItem) FilterValue() string { return i.name }

// poolItem is a row in the Pool (endpoints) list.
type poolItem struct {
	name    string
	status  string
	models  int
	prio    int
	reqs    int
	conns   int
	latency string
	image   string
}

func (i poolItem) Title() string { return i.name }
func (i poolItem) Description() string {
	return fmt.Sprintf("%s · img %s · %d models · prio %d · %d req · %d conns · %s",
		i.status, dashIf(i.image), i.models, i.prio, i.reqs, i.conns, dashIf(i.latency))
}
func (i poolItem) FilterValue() string { return i.name }

// vmItem is a row in the Nutanix VM list.
type vmItem struct {
	name  string
	role  string
	power string
	ip    string
	vcpu  int
	mem   float64
	disk  float64
	image string
}

func (i vmItem) Title() string { return i.name }
func (i vmItem) Description() string {
	return fmt.Sprintf("%s · %s · %s · %dvCPU %.0fGi mem %.0fGi disk · img %s",
		i.role, i.power, i.ip, i.vcpu, i.mem, i.disk, dashIf(i.image))
}
func (i vmItem) FilterValue() string { return i.name }

// agentItem is a row in the Agents list.
type agentItem struct {
	name       string
	cli        string
	target     string
	endpoint   string
	desc       string
	canDeploy  bool
	registered bool
	regHost    string
}

func (i agentItem) Title() string {
	if i.registered {
		return "✓ " + i.name + "  (registered)"
	}
	return "⬡ " + i.name
}
func (i agentItem) Description() string {
	if i.registered {
		return fmt.Sprintf("%s · %s · deployed on %s", i.desc, i.endpoint, i.regHost)
	}
	state := "preinstalled"
	if i.canDeploy {
		state = "deployable"
	}
	return fmt.Sprintf("%s · %s · %s · %s", i.desc, i.target, i.endpoint, state)
}
func (i agentItem) FilterValue() string { return i.name }

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

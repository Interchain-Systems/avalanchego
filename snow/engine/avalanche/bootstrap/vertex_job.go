// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package bootstrap

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ava-labs/avalanche-go/ids"
	"github.com/ava-labs/avalanche-go/snow/choices"
	"github.com/ava-labs/avalanche-go/snow/consensus/avalanche"
	"github.com/ava-labs/avalanche-go/snow/consensus/snowstorm"
	"github.com/ava-labs/avalanche-go/snow/engine/avalanche/vertex"
	"github.com/ava-labs/avalanche-go/snow/engine/common/queue"
	"github.com/ava-labs/avalanche-go/utils/logging"
)

type vtxParser struct {
	log                     logging.Logger
	numAccepted, numDropped prometheus.Counter
	mgr                     vertex.Manager
	snowstorm.TxManager
}

func (p *vtxParser) Parse(vtxBytes []byte) (queue.Job, error) {
	vtx, err := p.mgr.ParseVertex(vtxBytes)
	if err != nil {
		return nil, err
	}
	return &vertexJob{
		log:         p.log,
		numAccepted: p.numAccepted,
		numDropped:  p.numDropped,
		vtx:         vtx,
		mgr:         p.mgr,
		TxManager:   p.TxManager,
	}, nil
}

type vertexJob struct {
	log                     logging.Logger
	numAccepted, numDropped prometheus.Counter
	vtx                     avalanche.Vertex
	mgr                     vertex.Manager
	snowstorm.TxManager
}

func (v *vertexJob) ID() ids.ID { return v.vtx.ID() }

func (v *vertexJob) MissingDependencies() (ids.Set, error) {
	missing := ids.Set{}
	parentIDs, err := v.vtx.Parents()
	if err != nil {
		return missing, err
	}
	for _, parentID := range parentIDs {
		parent, err := v.mgr.GetVertex(parentID)
		if err != nil || parent.Status() != choices.Accepted {
			missing.Add(parentID)
		}
	}
	return missing, nil
}

func (v *vertexJob) Execute() error {
	deps, err := v.MissingDependencies()
	if err != nil {
		return err
	}
	if deps.Len() != 0 {
		v.numDropped.Inc()
		return errors.New("attempting to execute blocked vertex")
	}
	txs, err := v.vtx.Txs()
	if err != nil {
		return err
	}

	for _, tx := range txs {
		tx, err := v.GetTx(tx.ID())
		if err != nil {
			v.log.Warn("couldn't find latest version of tx %s", tx.ID())
		}
		if tx.Status() != choices.Accepted {
			v.numDropped.Inc()
			v.log.Warn("attempting to execute vertex %s with non-accepted transaction %s (has status %s)", v.vtx.ID(), tx.ID(), tx.Status())
			return nil
		}
	}
	status := v.vtx.Status()
	switch status {
	case choices.Unknown, choices.Rejected:
		v.numDropped.Inc()
		return fmt.Errorf("attempting to execute vertex with status %s", status)
	case choices.Processing:
		v.numAccepted.Inc()
		if err := v.vtx.Accept(); err != nil {
			return fmt.Errorf("failed to accept vertex in bootstrapping: %w", err)
		} else if err := v.mgr.SaveVertex(v.vtx); err != nil {
			return fmt.Errorf("failed to save block %s to VM's database: %s", v.vtx.ID(), err)
		}
	}
	return nil
}

func (v *vertexJob) Bytes() []byte { return v.vtx.Bytes() }

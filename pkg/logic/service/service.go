package service

import (
	"context"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"

	"github.com/inconshreveable/log15"
)

type KeibidropServiceImpl struct {
	bindings.UnimplementedKeibiServiceServer
	Session *session.Session
	Logger  log15.Logger
}

func (kd *KeibidropServiceImpl) Debug(context.Context, *bindings.DebugRequest) (*bindings.DebugResponse, error) {
	kd.Logger.Info("WAWAWAWAWWEAWEAWEAWEDAEWAWEDAWE")

	return &bindings.DebugResponse{}, nil
}

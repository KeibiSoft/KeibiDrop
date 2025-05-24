package service

import (
	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/inconshreveable/log15"
)

type KeibidropServiceImpl struct {
	bindings.UnimplementedKeibiServiceServer
	Session *session.Session
	Logger  log15.Logger
}

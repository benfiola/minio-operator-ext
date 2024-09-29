package operator

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

type OperatorTestSuite struct {
	suite.Suite
}

func (s *OperatorTestSuite) SetupSuite() {

}

func (s *OperatorTestSuite) SetupSubTest() {

}

func TestOperatorTestSuite(t *testing.T) {
	suite.Run(t, new(OperatorTestSuite))
}

func (s *OperatorTestSuite) TestMinioBucketReconciler() {
	s.T().Run("adds reconciler", func(t *testing.T) {

	})
}

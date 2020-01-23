// Code generated by mockery v1.0.0. DO NOT EDIT.

package mocks

import (
	models "chainlink/core/store/models"

	mock "github.com/stretchr/testify/mock"
)

// FluxMonitor is an autogenerated mock type for the FluxMonitor type
type FluxMonitor struct {
	mock.Mock
}

// AddJob provides a mock function with given fields: _a0
func (_m *FluxMonitor) AddJob(_a0 models.JobSpec) error {
	ret := _m.Called(_a0)

	var r0 error
	if rf, ok := ret.Get(0).(func(models.JobSpec) error); ok {
		r0 = rf(_a0)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Connect provides a mock function with given fields: _a0
func (_m *FluxMonitor) Connect(_a0 *models.Head) error {
	ret := _m.Called(_a0)

	var r0 error
	if rf, ok := ret.Get(0).(func(*models.Head) error); ok {
		r0 = rf(_a0)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Disconnect provides a mock function with given fields:
func (_m *FluxMonitor) Disconnect() {
	_m.Called()
}

// OnNewHead provides a mock function with given fields: _a0
func (_m *FluxMonitor) OnNewHead(_a0 *models.Head) {
	_m.Called(_a0)
}

// RemoveJob provides a mock function with given fields: _a0
func (_m *FluxMonitor) RemoveJob(_a0 *models.ID) {
	_m.Called(_a0)
}

// Start provides a mock function with given fields:
func (_m *FluxMonitor) Start() error {
	ret := _m.Called()

	var r0 error
	if rf, ok := ret.Get(0).(func() error); ok {
		r0 = rf()
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Stop provides a mock function with given fields:
func (_m *FluxMonitor) Stop() {
	_m.Called()
}

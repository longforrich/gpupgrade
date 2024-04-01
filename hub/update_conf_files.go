// Copyright (c) 2017-2023 VMware, Inc. or its affiliates
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/blang/semver/v4"
	"golang.org/x/xerrors"

	"github.com/greenplum-db/gpupgrade/greenplum"
	"github.com/greenplum-db/gpupgrade/idl"
	"github.com/greenplum-db/gpupgrade/step"
	"github.com/greenplum-db/gpupgrade/utils/errorlist"
)

func UpdateConfFiles(agentConns []*idl.Connection, _ step.OutStreams, version semver.Version, intermediate *greenplum.Cluster, target *greenplum.Cluster) error {
	if version.Major < 7 {
		// update gpperfmon.conf on coordinator
		err := UpdateConfigurationFile([]*idl.UpdateFileConfOptions{{
			Path:        filepath.Join(target.CoordinatorDataDir(), "gpperfmon", "conf", "gpperfmon.conf"),
			Pattern:     `^log_location = .*$`,
			Replacement: fmt.Sprintf("log_location = %s", filepath.Join(target.CoordinatorDataDir(), "gpperfmon", "logs")),
		}})
		if err != nil {
			return err
		}
	}

	// update postgresql.conf on coordinator
	err := UpdateConfigurationFile([]*idl.UpdateFileConfOptions{{
		Path:        filepath.Join(target.CoordinatorDataDir(), "postgresql.conf"),
		Pattern:     fmt.Sprintf(`(^port[ \t]*=[ \t]*)%d([^0-9]|$)`, intermediate.CoordinatorPort()),
		Replacement: fmt.Sprintf(`\1%d\2`, target.CoordinatorPort()),
	}})
	if err != nil {
		return err
	}

	if err := UpdatePostgresqlConfOnSegments(agentConns, intermediate, target); err != nil {
		return err
	}

	if err := UpdateRecoveryConfOnSegments(agentConns, version, intermediate, target); err != nil {
		return err
	}

	return nil
}

func UpdatePostgresqlConfOnSegments(agentConns []*idl.Connection, intermediate *greenplum.Cluster, target *greenplum.Cluster) error {
	pattern := `(^port[ \t]*=[ \t]*)%d([^0-9]|$)`
	replacement := `\1%d\2`

	request := func(conn *idl.Connection) error {
		var opts []*idl.UpdateFileConfOptions

		// add standby
		if target.Standby().Hostname == conn.Hostname {
			opt := &idl.UpdateFileConfOptions{
				Path:        filepath.Join(target.StandbyDataDir(), "postgresql.conf"),
				Pattern:     fmt.Sprintf(pattern, intermediate.StandbyPort()),
				Replacement: fmt.Sprintf(replacement, target.StandbyPort()),
			}

			opts = append(opts, opt)
		}

		// add mirrors
		mirrors := target.SelectSegments(func(seg *greenplum.SegConfig) bool {
			return seg.IsOnHost(conn.Hostname) && seg.IsMirror()
		})

		for _, mirror := range mirrors {
			opt := &idl.UpdateFileConfOptions{
				Path:        filepath.Join(mirror.DataDir, "postgresql.conf"),
				Pattern:     fmt.Sprintf(pattern, intermediate.Mirrors[mirror.ContentID].Port),
				Replacement: fmt.Sprintf(replacement, mirror.Port),
			}

			opts = append(opts, opt)
		}

		// add primaries
		primaries := target.SelectSegments(func(seg *greenplum.SegConfig) bool {
			return seg.IsOnHost(conn.Hostname) && seg.IsPrimary()
		})

		for _, primary := range primaries {
			opt := &idl.UpdateFileConfOptions{
				Path:        filepath.Join(primary.DataDir, "postgresql.conf"),
				Pattern:     fmt.Sprintf(pattern, intermediate.Primaries[primary.ContentID].Port),
				Replacement: fmt.Sprintf(replacement, primary.Port),
			}

			opts = append(opts, opt)
		}

		req := &idl.UpdateConfigurationRequest{Options: opts}
		_, err := conn.AgentClient.UpdateConfiguration(context.Background(), req)
		return err
	}

	return ExecuteRPC(agentConns, request)
}

func UpdateRecoveryConfOnSegments(agentConns []*idl.Connection, version semver.Version, intermediateCluster *greenplum.Cluster, target *greenplum.Cluster) error {
	file := "postgresql.auto.conf"
	if version.Major == 6 {
		file = "recovery.conf"
	}

	pattern := `(primary_conninfo .* port[ \t]*=[ \t]*)%d([^0-9]|$)`
	replacement := `\1%d\2`

	request := func(conn *idl.Connection) error {
		var opts []*idl.UpdateFileConfOptions

		// add standby
		if target.Standby().Hostname == conn.Hostname {
			opt := &idl.UpdateFileConfOptions{
				Path:        filepath.Join(target.StandbyDataDir(), file),
				Pattern:     fmt.Sprintf(pattern, intermediateCluster.CoordinatorPort()),
				Replacement: fmt.Sprintf(replacement, target.CoordinatorPort()),
			}

			opts = append(opts, opt)
		}

		// add mirrors
		mirrors := target.SelectSegments(func(seg *greenplum.SegConfig) bool {
			return seg.IsOnHost(conn.Hostname) && seg.IsMirror()
		})

		for _, mirror := range mirrors {
			opt := &idl.UpdateFileConfOptions{
				Path:        filepath.Join(mirror.DataDir, file),
				Pattern:     fmt.Sprintf(pattern, intermediateCluster.Primaries[mirror.ContentID].Port),
				Replacement: fmt.Sprintf(replacement, target.Primaries[mirror.ContentID].Port),
			}

			opts = append(opts, opt)
		}

		req := &idl.UpdateConfigurationRequest{Options: opts}
		_, err := conn.AgentClient.UpdateConfiguration(context.Background(), req)
		return err
	}

	return ExecuteRPC(agentConns, request)
}

func UpdateInternalAutoConfOnMirrors(agentConns []*idl.Connection, intermediate *greenplum.Cluster) error {
	pattern := `(^gp_dbid=)%d([^0-9]|$)`
	replacement := `\1%d\2`

	request := func(conn *idl.Connection) error {
		intermediateMirrors := intermediate.SelectSegments(func(seg *greenplum.SegConfig) bool {
			return seg.IsOnHost(conn.Hostname) && !seg.IsStandby() && seg.IsMirror()
		})

		if len(intermediateMirrors) == 0 {
			return nil
		}

		var opts []*idl.UpdateFileConfOptions
		for _, intermediateMirror := range intermediateMirrors {
			opt := &idl.UpdateFileConfOptions{
				Path:        filepath.Join(intermediateMirror.DataDir, "internal.auto.conf"),
				Pattern:     fmt.Sprintf(pattern, intermediate.Primaries[intermediateMirror.ContentID].DbID),
				Replacement: fmt.Sprintf(replacement, intermediateMirror.DbID),
			}

			opts = append(opts, opt)

		}

		req := &idl.UpdateConfigurationRequest{Options: opts}
		_, err := conn.AgentClient.UpdateConfiguration(context.Background(), req)
		return err
	}

	return ExecuteRPC(agentConns, request)
}

func UpdateConfigurationFile(opts []*idl.UpdateFileConfOptions) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(opts))

	for _, opt := range opts {

		wg.Add(1)
		go func(opt *idl.UpdateFileConfOptions) {
			defer wg.Done()

			cmd := exec.Command("sed", "-E", "-i.bak",
				fmt.Sprintf(`s@%s@%s@`, opt.GetPattern(), opt.GetReplacement()),
				opt.GetPath(),
			)

			output, err := cmd.CombinedOutput()
			if err != nil {
				errs <- xerrors.Errorf("update %s using %q failed with %q: %w", filepath.Base(opt.GetPath()), cmd.String(), string(output), err)
			}
		}(opt)
	}

	wg.Wait()
	close(errs)

	var err error
	for e := range errs {
		err = errorlist.Append(err, e)
	}

	return err
}

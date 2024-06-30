#!/usr/bin/env python3
import io
import os
import pathlib
import shlex
import subprocess
import sys
import threading

import click
import packaging.utils
import toml


def run_cmd(cmd: list[str], **kwargs):
    buffer_stdout = io.StringIO()
    buffer_stderr = io.StringIO()

    def _reader(role: str):
        nonlocal buffer_stdout, buffer_stderr, popen
        if role == "stdout":
            in_buffer = popen.stdout
            out_buffer = buffer_stdout
        elif role == "stderr":
            in_buffer = popen.stderr
            out_buffer = buffer_stderr
        else:
            raise NotImplementedError()

        def read():
            if in_buffer is None:
                raise RuntimeError()
            data = in_buffer.read()
            if data is None:
                return
            out_buffer.write(data)
            sys.stderr.write(data)

        while popen.returncode is not None:
            read()
        read()

    cmd_str = f"{shlex.join(cmd)}"
    if env := kwargs.get("env"):
        diff = {}
        for key, value in env.items():
            if os.environ.get(key) == value:
                continue
            diff[key] = value
        diff_str = " ".join(f"{k}={v}" for k, v in diff.items())
        if diff_str:
            cmd_str += f" (env: {diff_str})"
    if cwd := kwargs.get("cwd"):
        cmd_str += f" (cwd: {cwd})"

    print(f"$ {cmd_str}", file=sys.stderr)
    kwargs["encoding"] = "utf-8"
    kwargs["stdout"] = subprocess.PIPE
    kwargs["stderr"] = subprocess.PIPE
    popen = subprocess.Popen(cmd, **kwargs)
    readers = [
        threading.Thread(target=lambda: _reader("stdout")),
        threading.Thread(target=lambda: _reader("stderr")),
    ]
    [r.start() for r in readers]
    popen.wait()
    [r.join() for r in readers]

    buffer_stdout.seek(0)
    buffer_stderr.seek(0)
    stdout = buffer_stdout.read()
    stderr = buffer_stderr.read()
    if popen.returncode != 0:
        raise subprocess.CalledProcessError(
            cmd=cmd, output=stdout, returncode=popen.returncode, stderr=stderr
        )
    return stdout


def main():
    grp_main()


@click.group()
def grp_main():
    pass


@grp_main.command("build")
@click.option("--push", is_flag=True)
def cmd_build(*, push: bool = False):
    pyproject_file = pathlib.Path.cwd().joinpath("pyproject.toml")
    if not pyproject_file.exists():
        raise FileNotFoundError(pyproject_file)
    data = toml.loads(pyproject_file.read_text())
    version = data["project"]["version"]
    version = version.replace("+", "-")
    image = f"docker.io/benfiola/minio-operator-ext:{version}"

    command = [
        "docker",
        "buildx",
        "build",
        "--platform=linux/amd64,linux/arm64",
        "--progress=plain",
    ]
    if push:
        command.extend(["--push"])
    command.extend(["-t", image, "."])
    run_cmd(command)


@grp_main.command("publish")
@click.option("--token")
def cmd_publish(*, token: str):
    run_cmd(["docker", "login", "--username=benfiola", f"--password={token}"])
    run_cmd(["./scripts/helpers.py", "build", "--push"])


@grp_main.command("print-next-version")
@click.option("--as-tag", is_flag=True)
def cmd_print_next_version(*, as_tag: bool = False):
    command = ["python", "-m", "semantic_release", "--noop", "--strict", "version"]
    if as_tag is True:
        command.extend(["--print-tag"])
    else:
        command.extend(["--print"])
    env = {"GH_TOKEN": "undefined", **os.environ}
    version = run_cmd(command, env=env).strip()
    click.echo(version)


@grp_main.command("set-version")
@click.argument("version")
def cmd_set_version(*, version: str):
    pyproject_file = pathlib.Path.cwd().joinpath("pyproject.toml")
    if not pyproject_file.exists():
        raise FileNotFoundError(pyproject_file)
    data = toml.loads(pyproject_file.read_text())
    data["project"]["version"] = version
    pyproject_file.write_text(toml.dumps(data))


if __name__ == "__main__":
    main()

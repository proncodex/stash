import {
  faPause,
  faPlay,
  faStop,
  faTv,
} from "@fortawesome/free-solid-svg-icons";
import React, { useCallback, useEffect, useRef, useState } from "react";
import { Button, ButtonGroup, Overlay, Popover } from "react-bootstrap";
import { FormattedMessage, useIntl } from "react-intl";
import { Link } from "react-router-dom";
import { Icon } from "src/components/Shared/Icon";
import { getPlayer } from "src/components/ScenePlayer/util";
import { getPlatformURL } from "src/core/createClient";
import { SceneDataFragment } from "src/core/generated-graphql";
import { useToast } from "src/hooks/Toast";
import TextUtils from "src/utils/text";

export interface ICastToTVButtonProps {
  scene: SceneDataFragment;
}

interface ICastStatus {
  scene_id: string;
  title: string;
  time: number;
  duration: number;
  paused: boolean;
}

type CastCommand =
  | { action: "play"; scene_id: string; time: number }
  | { action: "pause" | "resume" | "stop" }
  | { action: "seek"; time: number };

const STATUS_POLL_MS = 1000;
const SKIPS = [-30, -10, 10, 30];

async function sendCommand(cmd: CastCommand) {
  const res = await fetch(getPlatformURL("tv/cast").toString(), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify(cmd),
  });
  if (!res.ok) {
    throw new Error(`${res.status} ${await res.text()}`);
  }
}

async function fetchStatus(): Promise<ICastStatus | null> {
  const res = await fetch(getPlatformURL("tv/cast/status").toString(), {
    credentials: "same-origin",
    cache: "no-store",
  });
  if (!res.ok) {
    throw new Error(`${res.status} ${await res.text()}`);
  }
  return res.json();
}

// Casts the scene to any browser that has the /tv page open, and acts as a
// remote control for whatever the TV is playing.
export const CastToTVButton: React.FC<ICastToTVButtonProps> = ({ scene }) => {
  const intl = useIntl();
  const Toast = useToast();
  const target = useRef(null);
  const [show, setShow] = useState(false);
  const [status, setStatus] = useState<ICastStatus | null>(null);
  // position of the seek slider while it is being dragged
  const [seekValue, setSeekValue] = useState<number>();

  const refresh = useCallback(async () => {
    try {
      setStatus(await fetchStatus());
    } catch {
      setStatus(null);
    }
  }, []);

  useEffect(() => {
    if (!show) return;
    refresh();
    const interval = setInterval(refresh, STATUS_POLL_MS);
    return () => clearInterval(interval);
  }, [show, refresh]);

  async function run(cmd: CastCommand) {
    try {
      await sendCommand(cmd);
    } catch (e) {
      Toast.error(e);
    }
    refresh();
  }

  async function castThisScene() {
    const player = getPlayer();
    await run({
      action: "play",
      scene_id: scene.id,
      time: player?.currentTime() ?? 0,
    });
    player?.pause();
    Toast.success(intl.formatMessage({ id: "toast.cast_to_tv" }));
  }

  function seekTo(time: number) {
    const max = status?.duration || Infinity;
    run({ action: "seek", time: Math.min(Math.max(0, time), max) });
  }

  // called when the slider is released. Touch drags end with pointercancel
  // rather than pointerup, so listen for touchend and mouseup separately.
  function commitSeek(e: React.SyntheticEvent<HTMLInputElement>) {
    if (seekValue !== undefined) {
      seekTo(Number(e.currentTarget.value));
      setSeekValue(undefined);
    }
  }

  function renderRemote() {
    if (!status) {
      return (
        <p className="text-muted mb-2">
          <FormattedMessage
            id="tv_remote.nothing_playing"
            values={{ url: <code>{getPlatformURL("tv").toString()}</code> }}
          />
        </p>
      );
    }

    const position = seekValue ?? status.time;

    return (
      <div className="mb-2">
        <div className="small text-muted">
          <FormattedMessage id="tv_remote.now_playing" />
        </div>
        <div className="text-truncate mb-1">
          <Link to={`/scenes/${status.scene_id}`}>{status.title}</Link>
        </div>
        {status.duration > 0 && (
          <>
            <input
              type="range"
              className="cast-seek"
              min={0}
              max={Math.floor(status.duration)}
              step={1}
              value={Math.floor(position)}
              onChange={(e) => setSeekValue(Number(e.currentTarget.value))}
              onMouseUp={commitSeek}
              onTouchEnd={commitSeek}
              onKeyUp={commitSeek}
            />
            <div className="d-flex justify-content-between small">
              <span>{TextUtils.secondsToTimestamp(position)}</span>
              <span>{TextUtils.secondsToTimestamp(status.duration)}</span>
            </div>
          </>
        )}
        <ButtonGroup className="d-flex mt-2">
          {SKIPS.slice(0, 2).map((s) => (
            <Button
              key={s}
              variant="secondary"
              onClick={() => seekTo(status.time + s)}
            >
              {s}s
            </Button>
          ))}
          <Button
            variant="primary"
            title={intl.formatMessage({
              id: status.paused ? "actions.play" : "actions.pause",
            })}
            onClick={() => run({ action: status.paused ? "resume" : "pause" })}
          >
            <Icon icon={status.paused ? faPlay : faPause} />
          </Button>
          {SKIPS.slice(2).map((s) => (
            <Button
              key={s}
              variant="secondary"
              onClick={() => seekTo(status.time + s)}
            >
              +{s}s
            </Button>
          ))}
          <Button
            variant="danger"
            title={intl.formatMessage({ id: "actions.stop" })}
            onClick={() => run({ action: "stop" })}
          >
            <Icon icon={faStop} />
          </Button>
        </ButtonGroup>
      </div>
    );
  }

  return (
    <>
      <Button
        ref={target}
        className="minimal px-0 px-sm-2 pt-2"
        variant="secondary"
        onClick={() => setShow(!show)}
        title={intl.formatMessage({ id: "actions.cast_to_tv" })}
      >
        <Icon icon={faTv} />
      </Button>
      <Overlay
        target={target.current}
        show={show}
        placement="bottom"
        rootClose
        onHide={() => setShow(false)}
      >
        <Popover id="cast-to-tv-popover" className="cast-to-tv-popover">
          <Popover.Content>
            {renderRemote()}
            <Button block variant="primary" onClick={castThisScene}>
              <Icon icon={faTv} />{" "}
              <FormattedMessage id="tv_remote.cast_this_scene" />
            </Button>
          </Popover.Content>
        </Popover>
      </Overlay>
    </>
  );
};

export default CastToTVButton;

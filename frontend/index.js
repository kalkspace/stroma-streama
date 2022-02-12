import { JSONParser } from "@streamparser/json";

/** @ts-ignore */
import playButtonImage from "./images/stroma-play.svg";
/** @ts-ignore */
import pauseButtonImage from "./images/stroma-pause.svg";
/** @ts-ignore */
import loadingButtonImage from "./images/stroma-loading.svg";

const BACKEND_URL = "https://marcus.stromaproxy.kalk.space/sdp";

async function parseJsonObjectStream(stream, handler) {
  const parser = new JSONParser({ paths: ["$"], separator: "" });
  parser.onValue = handler;

  const reader = stream.getReader();
  const parse = async () => {
    const { done, value } = await reader.read();
    if (done) {
      parser.end();
      return;
    }
    try {
      parser.write(value);
    } catch (e) {
      console.error("failed to parse json", e);
      return;
    }
    await parse();
  };
  await parse();
}

/**
 * @returns {Promise<RTCPeerConnection>}
 */
async function initWebRTC() {
  const peerConn = new RTCPeerConnection({
    iceServers: [
      { urls: "stun:bbb.kalk.space:5349" },
      {
        urls: [
          "turns:bbb.kalk.space:5349",
          "turn:bbb.kalk.space:5349?transport=tcp",
          "turn:bbb.kalk.space:5349",
        ],
        username: "webrtc",
        credential: "pengGUT1",
      },
    ],
  });
  // Offer to receive 1 audio
  peerConn.addTransceiver("audio", { direction: "sendrecv" });

  peerConn.addEventListener("iceconnectionstatechange", () =>
    console.debug("connection state change:", peerConn.iceConnectionState)
  );

  const iceCandidatesDone = new Promise((resolve) => {
    peerConn.addEventListener("icecandidate", ({ candidate }) => {
      console.debug("got ice candidate", { candidate });
      if (candidate === null) {
        console.debug("ice candidates discovered");
        resolve();
      }
    });
  });

  const localDesc = await peerConn.createOffer();
  await peerConn.setLocalDescription(localDesc);

  await iceCandidatesDone;

  return peerConn;
}

/** @typedef {"idle" | "loading" | "playing"} PlayerState */
/** @type {PlayerState} */
let playerState = "idle";
/** @type {Record<PlayerState, string>} */
const stateToImage = {
  idle: playButtonImage,
  loading: loadingButtonImage,
  playing: pauseButtonImage,
};
/**
 * @param {HTMLButtonElement} button
 * @param {PlayerState} newState
 */
function setState(button, newState) {
  let buttonImage = stateToImage[newState];
  button.style.backgroundImage = `url('${buttonImage}')`;
  playerState = newState;
}

/**
 * @param {RTCPeerConnection} peerConn
 * @returns {Promise<MediaStream>}
 */
async function startSession(peerConn) {
  /** @type {(track: MediaStream) => void} */
  let notifyReadyToPlay;
  /** @type {Promise<MediaStream>} */
  const readyToPlay = new Promise((resolve) => {
    notifyReadyToPlay = resolve;
  });
  peerConn.addEventListener("track", (event) => {
    const track = event.streams[0];
    console.debug("received track", { event, track });
    notifyReadyToPlay(track);
  });

  const response = await fetch(BACKEND_URL, {
    method: "POST",
    headers: {
      "content-type": "application/json",
    },
    body: JSON.stringify(peerConn.localDescription),
  });

  if (!response.ok) {
    console.error("http request failed:", response.status);
    return;
  }

  // not awaiting, can happen in the background
  parseJsonObjectStream(response.body, (value) => {
    console.debug("got json:", value);
    const { error } = value;
    if (error) {
      console.error("got error from backend:", value.error);
      return;
    }
    if ("sdp" in value) {
      // treat as remote description
      peerConn.setRemoteDescription(new RTCSessionDescription(value));
      console.log("set remote description");
      return;
    }
    if ("candidate" in value) {
      peerConn.addIceCandidate(value);
      return;
    }
    console.warn("received unexpected message", value);
  }).then(() => console.debug("parsed all json from body"));

  return readyToPlay;
}

/**
 * @param {HTMLAudioElement} player
 * @param {HTMLButtonElement} button
 * @param {Promise<RTCPeerConnection>} webrtcConnPromise
 */
async function handleButton(player, button, webrtcConnPromise) {
  const peerConn = await webrtcConnPromise;

  if (playerState === "idle") {
    if (peerConn.connectionState === "connected") {
      let track = peerConn.getReceivers()[0]?.track;
      if (track) {
        track.enabled = true;
        setState(button, "playing");
        return;
      }
    }

    setState(button, "loading");
    const [_, stream] = await Promise.all([
      player.play(), // plays silence
      startSession(peerConn),
    ]);
    console.debug("got stream, starting to play");
    player.srcObject = stream;
    await player.play();
    setState(button, "playing");
    return;
  }
  if (playerState === "loading") {
    return;
  }
  if (playerState === "playing") {
    let track = peerConn.getReceivers()[0]?.track;
    if (track) {
      track.enabled = false;
    }
    setState(button, "idle");
  }
}

/**
 * @param {HTMLOrSVGScriptElement} scriptTag
 */
function initEmbed(scriptTag) {
  const webrtcConnPromise = initWebRTC();

  const container = document.createElement("div");

  const player = document.createElement("audio");
  /** @ts-ignore */
  const silentUrl = new URL("1-second-of-silence.mp3", scriptTag.src);
  player.src = silentUrl.toString();
  container.appendChild(player);

  const playButton = document.createElement("button");
  playButton.style.backgroundColor = "transparent";
  playButton.style.backgroundImage = `url(${playButtonImage})`;
  playButton.style.backgroundSize = "contain";
  playButton.style.backgroundRepeat = "no-repeat";
  playButton.style.backgroundPosition = "center";
  playButton.style.border = "none";
  playButton.style.width = "200px";
  playButton.style.height = "200px";
  playButton.style.cursor = "pointer";
  playButton.addEventListener("click", () =>
    handleButton(player, playButton, webrtcConnPromise)
  );
  container.appendChild(playButton);

  player.addEventListener("play", () => setState(playButton, "playing"));

  scriptTag.parentNode.insertBefore(container, scriptTag);
}
initEmbed(document.currentScript);

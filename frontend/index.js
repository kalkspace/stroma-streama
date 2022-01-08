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

async function initWebRTC(player) {
  const peerConn = new RTCPeerConnection({
    iceServers: [{ urls: "stun:stun.l.google.com:19302" }],
  });
  // Offer to receive 1 audio
  peerConn.addTransceiver("audio", { direction: "sendrecv" });

  peerConn.addEventListener("track", (event) => {
    const track = event.streams[0];
    console.debug("received track", { event, track });
    player.srcObject = track;
    player.play();
  });
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
 */
async function startSession(peerConn) {
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

  await parseJsonObjectStream(response.body, (value) => {
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
  });
  console.debug("parsed all json from body");
}

/**
 * @param {HTMLAudioElement} player
 * @param {HTMLButtonElement} button
 */
async function handleButton(player, button) {
  if (playerState === "idle") {
    setState(button, "loading");
    await player.play();
    setState(button, "playing");
    return;
  }
  if (playerState === "loading") {
    return;
  }
  if (playerState === "playing") {
    setState(button, "idle");
    player.pause();
  }
}

/**
 * @param {Element} beforeTag
 * @param {string} srcURL
 */
function initEmbed(beforeTag, srcURL) {
  const container = document.createElement("div");

  const player = document.createElement("audio");
  let audioSrc = "/stroma-pink-noise.mp3";
  if (srcURL.match(/^https?:\/\//)) {
    audioSrc = new URL(audioSrc, srcURL).toString();
  }
  player.src = audioSrc;
  player.loop = true;
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
  playButton.addEventListener("click", () => handleButton(player, playButton));
  container.appendChild(playButton);

  beforeTag.parentNode.insertBefore(container, beforeTag);
}
initEmbed(document.currentScript, document.currentScript.getAttribute("src"));

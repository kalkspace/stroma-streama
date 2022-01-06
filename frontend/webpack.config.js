const path = require("path");

module.exports = {
  mode: "development",
  entry: path.resolve(__dirname, "./index.js"),
  module: {
    rules: [
      {
        test: /\.(js)$/,
        exclude: /node_modules/,
        use: ["babel-loader"],
      },
    ],
  },
  resolve: {
    extensions: ["*", ".js"],
  },
  output: {
    path: path.resolve(__dirname, "./dist"),
    filename: "embed.js",
  },
  devServer: {
    static: {
      directory: path.join(__dirname, "dist"),
    },
  },
};

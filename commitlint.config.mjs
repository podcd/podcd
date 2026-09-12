export default {
  extends: ["@commitlint/config-conventional"],
  rules: {
    // Subjects often start with an acronym (PR, README, CLI, YAML, SSH), which
    // the default rule mistakes for sentence case. The type and format are
    // what release notes depend on; the casing of the subject is not.
    "subject-case": [0],
  },
};
